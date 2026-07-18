"""Asker cross-encoder reranker model service (Phase 1).

Serves a cross-encoder (bge-reranker-v2-m3 by default) over HTTP: given a query
and a batch of candidate documents, it returns one relevance score per document.
The query service calls it to reorder the top fused retrieval candidates before
the final ordering. Mirrors the structure of services/clip.
"""
