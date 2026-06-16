import { useCallback, useEffect, useMemo, useRef, useState } from "react";

// Minimal typings for the Web Speech API (SpeechRecognition). The webkit-
// prefixed form is not in lib.dom, so we declare just the slice we use rather
// than pull in @types for a browser-only, progressively-enhanced feature.
interface SpeechAlternative {
  transcript: string;
}
interface SpeechResult {
  readonly isFinal: boolean;
  readonly length: number;
  readonly [index: number]: SpeechAlternative;
}
interface SpeechResultList {
  readonly length: number;
  readonly [index: number]: SpeechResult;
}
interface SpeechRecognitionEventLike {
  readonly resultIndex: number;
  readonly results: SpeechResultList;
}
interface SpeechRecognitionErrorEventLike {
  readonly error: string;
}
interface SpeechRecognitionLike {
  lang: string;
  interimResults: boolean;
  continuous: boolean;
  maxAlternatives: number;
  start(): void;
  stop(): void;
  abort(): void;
  onresult: ((e: SpeechRecognitionEventLike) => void) | null;
  onerror: ((e: SpeechRecognitionErrorEventLike) => void) | null;
  onend: (() => void) | null;
}
type SpeechRecognitionCtor = new () => SpeechRecognitionLike;

/** The SpeechRecognition constructor (standard or webkit-prefixed), or null when
 * the browser doesn't support voice input. */
function recognitionCtor(): SpeechRecognitionCtor | null {
  const w = window as unknown as {
    SpeechRecognition?: SpeechRecognitionCtor;
    webkitSpeechRecognition?: SpeechRecognitionCtor;
  };
  return w.SpeechRecognition ?? w.webkitSpeechRecognition ?? null;
}

export interface VoiceInput {
  /** Whether the browser supports speech recognition (Web Speech API). */
  supported: boolean;
  /** True while actively listening. */
  listening: boolean;
  /** A short, human-readable error (e.g. mic permission denied), or "". */
  error: string;
  /** Start listening, or stop if already listening. */
  toggle: () => void;
}

export interface VoiceInputOptions {
  /** Called with the running transcript (interim + final) so the box can show
   * the words as they are spoken. */
  onTranscript: (text: string) => void;
  /** Called once with the final transcript when a phrase completes (used to
   * auto-submit the search, hands-free). */
  onFinal: (text: string) => void;
}

/**
 * useVoiceInput wires the search box's microphone to the Web Speech API: click
 * to dictate, the words stream into the box as you speak, and the search runs
 * automatically when you stop. Progressive enhancement — `supported` is false
 * (and the caller hides the mic) when the browser lacks the API.
 */
export function useVoiceInput({
  onTranscript,
  onFinal,
}: VoiceInputOptions): VoiceInput {
  const [listening, setListening] = useState(false);
  const [error, setError] = useState("");
  const recRef = useRef<SpeechRecognitionLike | null>(null);
  const supported = useMemo(() => recognitionCtor() !== null, []);

  // Keep the latest callbacks without re-creating start() each render.
  const cbRef = useRef<VoiceInputOptions>({ onTranscript, onFinal });
  cbRef.current = { onTranscript, onFinal };

  const stop = useCallback(() => {
    recRef.current?.stop();
  }, []);

  const start = useCallback(() => {
    const Ctor = recognitionCtor();
    if (!Ctor) {
      return;
    }
    recRef.current?.abort();
    const rec = new Ctor();
    rec.lang = navigator.language || "en-US";
    rec.interimResults = true;
    rec.continuous = false;
    rec.maxAlternatives = 1;

    let finalText = "";
    rec.onresult = (e) => {
      let interim = "";
      for (let i = e.resultIndex; i < e.results.length; i++) {
        const res = e.results[i];
        const phrase = res[0]?.transcript ?? "";
        if (res.isFinal) {
          finalText += phrase;
        } else {
          interim += phrase;
        }
      }
      cbRef.current.onTranscript((finalText + interim).trim());
    };
    rec.onerror = (e) => {
      // "no-speech"/"aborted" are normal end states, not user-facing errors.
      setError(e.error === "not-allowed" ? "Microphone permission denied" : "");
      setListening(false);
    };
    rec.onend = () => {
      setListening(false);
      recRef.current = null;
      const final = finalText.trim();
      if (final !== "") {
        cbRef.current.onFinal(final);
      }
    };

    recRef.current = rec;
    setError("");
    setListening(true);
    try {
      rec.start();
    } catch {
      // start() throws if called while already started; treat as no-op.
      setListening(false);
    }
  }, []);

  const toggle = useCallback(() => {
    if (listening) {
      stop();
    } else {
      start();
    }
  }, [listening, start, stop]);

  // Abort any in-flight recognition on unmount so the mic is released.
  useEffect(() => () => recRef.current?.abort(), []);

  return { supported, listening, error, toggle };
}
