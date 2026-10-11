# lite-model-router

A tiny reverse proxy for OpenAI- and Anthropic-compatible model endpoints, with **virtual models**: one model name that resolves to an ordered fallback chain of real backends (e.g. "use the big GPU box when it's online, otherwise the always-on mini PC"). Minimal dependencies, a single binary, works for chat, vision, embeddings and any other endpoint the backends expose.

Status: early development. See `docs/PLAN.md` once it lands.
