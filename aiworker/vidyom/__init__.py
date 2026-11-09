"""Vidyom AI worker (D31, I6): transcription and embeddings over JSON-RPC 2.0.

The wire contract is schemas/ai-worker-rpc.v1.json. Redis, storage and secrets
stay out of this process: it reads local files named by its clients and answers.
"""

__version__ = "0.0.0.dev0"
