"""jdix-sandbox: isolated code execution for AI agents.

    from jdix import Client

    with Client().create("py312-small", ttl=600) as sbx:
        result = sbx.run("pip install requests && python -c 'import requests'")
        print(result.exit_code, result.stdout)

        sbx.files.write("/workspace/main.py", "print('hi')")
        for chunk in sbx.run_stream("python main.py"):
            print(chunk.text, end="")

        url = sbx.expose(8000)
"""

from ._errors import (
    AuthenticationError,
    JdixError,
    MountRejected,
    NotFound,
    PermissionDenied,
    QuotaExceeded,
    SandboxExpired,
    SandboxFailed,
    SandboxTimeout,
)
from ._models import Chunk, FileInfo, Mount, Process, Result, SandboxInfo, Template
from ._sandbox import Client, Sandbox

__version__ = "0.1.0"

__all__ = [
    "Client",
    "Sandbox",
    "Mount",
    "Result",
    "Chunk",
    "FileInfo",
    "Process",
    "Template",
    "SandboxInfo",
    "JdixError",
    "AuthenticationError",
    "PermissionDenied",
    "NotFound",
    "QuotaExceeded",
    "SandboxExpired",
    "SandboxFailed",
    "SandboxTimeout",
    "MountRejected",
]
