// Lazily-loaded thumbnail for a media hit.
//
// <img> cannot send an Authorization header, so the bytes are fetched via the
// MediaClient (bearer token) into an object URL we assign to `src`. The fetch
// runs in an effect so it never blocks the results list from rendering; we
// show a placeholder until it resolves and revoke the object URL on unmount
// (or when the key changes) to avoid leaking blob references.
import { useEffect, useState } from "react";
import { isAbortError } from "../api";

/** Fetches a thumbnail blob by key and returns an object URL. */
export type FetchThumbnail = (
  key: string,
  signal?: AbortSignal,
) => Promise<string>;

export function Thumbnail({
  thumbnailKey,
  alt,
  fetchThumbnail,
}: {
  thumbnailKey: string;
  alt: string;
  fetchThumbnail: FetchThumbnail;
}) {
  const [url, setUrl] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    if (thumbnailKey === "") {
      return;
    }
    let objectUrl: string | null = null;
    const controller = new AbortController();
    setUrl(null);
    setFailed(false);
    fetchThumbnail(thumbnailKey, controller.signal)
      .then((u) => {
        objectUrl = u;
        setUrl(u);
      })
      .catch((err: unknown) => {
        if (isAbortError(err)) {
          return; // unmounted / key changed
        }
        setFailed(true);
      });
    return () => {
      controller.abort();
      if (objectUrl !== null) {
        URL.revokeObjectURL(objectUrl);
      }
    };
  }, [thumbnailKey, fetchThumbnail]);

  if (failed) {
    return (
      <div className="thumb thumb-fallback" role="img" aria-label={alt}>
        <span aria-hidden="true">🖼</span>
      </div>
    );
  }

  if (url === null) {
    return (
      <div
        className="thumb thumb-placeholder"
        aria-busy="true"
        aria-label="Loading preview"
      />
    );
  }

  return <img className="thumb" src={url} alt={alt} loading="lazy" />;
}
