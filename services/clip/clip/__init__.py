"""Asker CLIP model service (ADR-013): one loaded open_clip model serving both
text and image encoders over HTTP so the two vector spaces coincide.

POST /embed/text  {"inputs":[...]}      -> {"embeddings":[[float x CLIP_DIM], ...]}
POST /embed/image {"images_b64":[...]}  -> {"embeddings":[[float x CLIP_DIM], ...]}
GET  /health                            -> 200 once the model is loaded.
"""
