// Test-only fixture matching the pinned /v1/search response shape.
// This module must ONLY be imported from *.test.* files — there is no runtime
// mock switch; the app always talks to the real gateway.
import type { SearchResponse } from "../api";

export const searchMock: SearchResponse = {
  hits: [
    {
      doc_id: "f3a1c5e7d9b2a4c6e8f0a1b3c5d7e9f1a3b5c7d9e1f3a5b7c9d1e3f5a7b9c1d3",
      connector_id: "gmail",
      type: "EMAIL",
      title: "Q3 planning notes",
      snippet:
        "Discussed the <hi>quarterly</hi> roadmap and the <hi>planning</hi> cadence for the team.",
      score: 0.91,
      created: "2026-05-02T09:15:00Z",
      modified: "2026-05-03T11:42:00Z",
      metadata: { thread_id: "187cdeadbeef" },
      source_url: "https://mail.google.com/mail/u/0/#all/187cdeadbeef",
    },
    {
      doc_id: "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90",
      connector_id: "upload",
      type: "FILE",
      title: "design-doc.pdf",
      snippet: "Initial <hi>planning</hi> draft for the search architecture.",
      score: 0.74,
      created: "2026-04-20T16:00:00Z",
      modified: "",
      metadata: {},
    },
  ],
  total: 42,
  degraded: "",
  took_ms: 87,
  cached: false,
};

/** An IMAGE hit whose OCR text matched (carries a thumbnail). */
export const imageHitMock: SearchResponse["hits"][number] = {
  doc_id: "img00000000000000000000000000000000000000000000000000000000000001",
  connector_id: "upload",
  type: "IMAGE",
  title: "whiteboard-sketch.png",
  snippet: "Whiteboard with the <hi>roadmap</hi> diagram.",
  score: 0.83,
  created: "2026-05-10T12:00:00Z",
  modified: "",
  metadata: {},
  start_ms: 0,
  end_ms: 0,
  modality: "ocr",
  thumbnail_key: "tenants/t1/thumbs/img1.jpg",
};

/** A VIDEO hit matched on an ASR transcript chunk at 1:23 (83000ms). */
export const videoHitMock: SearchResponse["hits"][number] = {
  doc_id: "vid00000000000000000000000000000000000000000000000000000000000002",
  connector_id: "upload",
  type: "VIDEO",
  title: "all-hands.mp4",
  snippet: "...and then we discussed the <hi>quarterly</hi> targets...",
  score: 0.79,
  created: "2026-05-11T09:00:00Z",
  modified: "",
  metadata: {},
  start_ms: 83000,
  end_ms: 91000,
  modality: "asr",
  thumbnail_key: "tenants/t1/thumbs/vid2.jpg",
};

/** Build a Response carrying `searchMock` (or an override) as JSON. */
export function mockSearchResponse(body: SearchResponse = searchMock): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
  });
}

/** Build an error Response in the gateway's {"error": "..."} shape. */
export function mockErrorResponse(status: number, error: string): Response {
  return new Response(JSON.stringify({ error }), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}
