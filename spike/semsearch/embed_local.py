#!/usr/bin/env python3
"""Local embedder for the semsearch spike — reads {"input": [...]} on stdin and
writes {"data": [{"index": i, "embedding": [...]}]} on stdout, mirroring the
OpenAI embeddings response so the Go harness can call it the same way.

Uses fastembed (ONNX runtime, no PyTorch) with BAAI/bge-small-en-v1.5: a real,
canonical *small local* embedding model (384-dim, ~130 MB) — the kind a
production opt-in semantic search would actually bundle. Loads the model once
per invocation, so the harness calls it with all texts in a single batch.
"""
import json
import sys

from fastembed import TextEmbedding

MODEL = "BAAI/bge-small-en-v1.5"


def main() -> None:
    req = json.load(sys.stdin)
    texts = req["input"]
    model = TextEmbedding(model_name=MODEL)
    vecs = list(model.embed(texts))
    data = [{"index": i, "embedding": [float(x) for x in v]} for i, v in enumerate(vecs)]
    json.dump({"data": data}, sys.stdout)


if __name__ == "__main__":
    main()
