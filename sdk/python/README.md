# jdix-sandbox (Python)

Isolated code execution for AI agents. Start a sandbox, run things in it, take
the results, and let it clean itself up.

```python
from jdix import Client

with Client().create("py312-small", ttl=600) as sbx:
    result = sbx.run("pip install requests && python -c 'import requests; print(requests.__version__)'")
    print(result.exit_code, result.stdout)

    sbx.files.write("/workspace/main.py", "print('hi')")
    for chunk in sbx.run_stream("python main.py"):
        print(chunk.text, end="")

    url = sbx.expose(8000)   # reachable from outside while the sandbox lives
```

Set `JDIX_API_KEY`, or pass `Client(api_key=...)`. `JDIX_BASE_URL` points at a
different installation.

## Two conventions worth knowing

**A failing command is not an exception.** `run()` returns a `Result` whose
`exit_code` says what happened. The request succeeded; the command failed. If
you prefer the raising shape, `result.check()` opts into it.

**Files stream.** `files.open()` is the primitive and `read_text()` is the
convenience, not the other way round — sandboxes produce artefacts far larger
than anything worth holding in memory.

## Mounting a volume

Templates declare which volumes exist; a sandbox picks a subpath of one. There
is no way to name a host path, which is the point.

```python
from jdix import Client, Mount

sbx = Client().create(
    "py312-small",
    mounts=[Mount(path="/data/corpus", volume="corpus", sub_path="2026-09", read_only=True)],
)
```

## Retries

Reads are retried on transport failures and 5xx, with jittered backoff.

Creates are never retried automatically, because the client does not invent an
idempotency key on your behalf — a silent retry is how a network hiccup starts
two sandboxes and bills for both. Pass `idempotency_key=` yourself whenever a
create might be replayed, and the server will return the first sandbox instead.

Quota errors (`QuotaExceeded`) are raised immediately rather than retried behind
`Retry-After`. A quota does not clear in the next few hundred milliseconds, and
the caller is better placed to decide what to do about it.

## Errors

```
JdixError
├── AuthenticationError   key missing, wrong, revoked or expired
├── PermissionDenied      valid key, not allowed to do this
├── NotFound              no such sandbox, template or file
├── QuotaExceeded         tenant limit hit; carries .retry_after
├── SandboxExpired        the TTL elapsed
├── SandboxFailed         the sandbox could not start; .message says why
├── MountRejected         the filesystem spec was refused; names the field
└── SandboxTimeout        a command exceeded its deadline
```

## Development

```sh
uv venv && uv pip install -e '.[dev]'
.venv/bin/python -m pytest
```
