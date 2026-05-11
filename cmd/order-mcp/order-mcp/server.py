"""MCP server exposing the indodax-bot `order` CLI as a tool.

Run:  uv run --with mcp python server.py
Or:   pip install mcp && python server.py

Configure in Claude Code (~/.claude.json or project .mcp.json):

  {
    "mcpServers": {
      "indodax-order": {
        "command": "python",
        "args": ["/Users/hajir/Public/strategies/indodax-bot/cmd/order-mcp/server.py"]
      }
    }
  }
"""
from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

from mcp.server.fastmcp import FastMCP

REPO = Path(__file__).resolve().parents[2]  # indodax-bot/

mcp = FastMCP("indodax-order")


@mcp.tool()
def place_order(
    direction: str,
    pair: str,
    amount: float,
    price: float,
    timeout_sec: int = 60,
    cancel_on_timeout: bool = True,
    warmup_sec: float = 1.0,
    log_path: str = "../logs/order-roundtrip.ndjson",
) -> str:
    """Place a single live order via the indodax-bot order CLI and return its stdout/stderr.

    Args:
        direction: "buy" or "sell".
        pair: e.g. "sol_idr", "btc_idr".
        amount: For buy = quote (IDR). For sell = base (e.g. SOL).
        price: Limit price in IDR.
        timeout_sec: Give up waiting for fill after this long.
        cancel_on_timeout: Cancel the order if not filled before timeout.
        warmup_sec: Delay after subscribing private WS before placing order.
        log_path: Append JSON record per run.
    """
    direction = direction.lower()
    if direction not in ("buy", "sell"):
        return f"error: direction must be buy|sell, got {direction!r}"

    cmd = [
        "go", "run", "./cmd/order",
        f"-timeout={timeout_sec}s",
        f"-cancel={'true' if cancel_on_timeout else 'false'}",
        f"-warmup={warmup_sec}s",
        f"-log={log_path}",
        direction, pair.lower(), str(amount), str(price),
    ]

    try:
        proc = subprocess.run(
            cmd,
            cwd=REPO,
            env=os.environ.copy(),
            capture_output=True,
            text=True,
            timeout=timeout_sec + 30,
        )
    except subprocess.TimeoutExpired as e:
        return f"timeout: process exceeded {timeout_sec + 30}s\nstdout:\n{e.stdout}\nstderr:\n{e.stderr}"

    return (
        f"exit={proc.returncode}\n"
        f"--- stdout ---\n{proc.stdout}\n"
        f"--- stderr ---\n{proc.stderr}"
    )


@mcp.tool()
def cancel_order(direction: str, pair: str, order_id: str) -> str:
    """Cancel an open order via the indodax-bot cancel_order CLI.

    Args:
        direction: "buy" or "sell" — must match the side the order was placed on.
        pair: e.g. "sol_idr".
        order_id: The order_id reported by place_order (or from /api/proxy/open-orders).
    """
    direction = direction.lower()
    if direction not in ("buy", "sell"):
        return f"error: direction must be buy|sell, got {direction!r}"

    cmd = ["go", "run", "./cmd/cancel_order", direction, pair.lower(), order_id]
    try:
        proc = subprocess.run(
            cmd,
            cwd=REPO,
            env=os.environ.copy(),
            capture_output=True,
            text=True,
            timeout=30,
        )
    except subprocess.TimeoutExpired as e:
        return f"timeout: cancel exceeded 30s\nstdout:\n{e.stdout}\nstderr:\n{e.stderr}"

    return (
        f"exit={proc.returncode}\n"
        f"--- stdout ---\n{proc.stdout}\n"
        f"--- stderr ---\n{proc.stderr}"
    )


@mcp.tool()
def tail_log(lines: int = 5, log_path: str = "../logs/order-roundtrip.ndjson") -> str:
    """Return the last N lines of a roundtrip NDJSON log.

    Args:
        lines: Number of trailing lines to return (default 5, max 200).
        log_path: Path relative to indodax-bot/ (default "../logs/order-roundtrip.ndjson",
                  which resolves to the centralized strategies/logs/ folder).
    """
    lines = max(1, min(int(lines), 200))
    path = (REPO / log_path).resolve() if not os.path.isabs(log_path) else Path(log_path)
    if not path.exists():
        return f"log not found: {path}"
    try:
        with path.open("rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            block = 8192
            data = b""
            while size > 0 and data.count(b"\n") <= lines:
                step = min(block, size)
                size -= step
                f.seek(size)
                data = f.read(step) + data
        tail = b"\n".join(data.splitlines()[-lines:]).decode("utf-8", errors="replace")
        return f"{path}\n{tail}"
    except OSError as e:
        return f"error reading {path}: {e}"


@mcp.tool()
def fetch_klines(
    pair: str,
    start: str = "2013-01-01",
    tf: str = "1",
    dir: str = "../data",
    timeout_sec: int = 3600,
) -> str:
    """Fetch and persist Indodax klines to <dir>/klines_<SYMBOL>_<tf>.jsonl.

    Paginates backward from the latest available bar (or the earliest stored
    bar, if a file already exists) until reaching `start`. Resume-aware.

    Args:
        pair: e.g. "btc_idr", "sol_idr".
        start: Stop paginating before this date (YYYY-MM-DD).
        tf: Timeframe — one of "1", "5", "15", "60", "1D".
        dir: Output directory relative to indodax-bot/ (default "../data").
        timeout_sec: Subprocess timeout.
    """
    if tf not in ("1", "5", "15", "60", "1D"):
        return f"error: tf must be 1|5|15|60|1D, got {tf!r}"

    cmd = [
        "go", "run", "./cmd/fetch-klines",
        f"-pair={pair.lower()}",
        f"-start={start}",
        f"-tf={tf}",
        f"-dir={dir}",
    ]
    try:
        proc = subprocess.run(
            cmd, cwd=REPO, env=os.environ.copy(),
            capture_output=True, text=True, timeout=timeout_sec,
        )
    except subprocess.TimeoutExpired as e:
        return f"timeout: process exceeded {timeout_sec}s\nstdout:\n{e.stdout}\nstderr:\n{e.stderr}"

    return (
        f"exit={proc.returncode}\n"
        f"--- stdout ---\n{proc.stdout}\n"
        f"--- stderr ---\n{proc.stderr}"
    )


@mcp.tool()
def read_klines(
    pair: str,
    tf: str = "1",
    last: int = 100,
    dir: str = "../data",
) -> str:
    """Return the last N persisted klines for a pair as JSON lines.

    Useful for quickly inspecting recent market state. Reads from
    <dir>/klines_<SYMBOL>_<tf>.jsonl (relative to indodax-bot/).

    Args:
        pair: e.g. "btc_idr".
        tf: Timeframe — one of "1", "5", "15", "60", "1D".
        last: Number of trailing bars to return (1..5000).
        dir: Data directory (default "../data").
    """
    last = max(1, min(int(last), 5000))
    symbol = pair.upper().replace("_", "")
    path = (REPO / dir / f"klines_{symbol}_{tf}.jsonl").resolve()
    if not path.exists():
        return f"file not found: {path}\n(hint: run fetch_klines first)"

    try:
        with path.open("rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            block = 65536
            data = b""
            while size > 0 and data.count(b"\n") <= last:
                step = min(block, size)
                size -= step
                f.seek(size)
                data = f.read(step) + data
        lines = data.splitlines()[-last:]
    except OSError as e:
        return f"error reading {path}: {e}"

    bars = []
    for ln in lines:
        try:
            bars.append(json.loads(ln))
        except json.JSONDecodeError:
            continue
    if not bars:
        return f"{path}\n(no parseable bars)"

    first_t = bars[0].get("OpenTime", "?")
    last_t = bars[-1].get("OpenTime", "?")
    head = f"{path}\n{len(bars)} bars | {first_t} → {last_t}\n"
    body = "\n".join(json.dumps(b, separators=(",", ":")) for b in bars)
    return head + body


@mcp.tool()
def dry_run_preview(direction: str, pair: str, amount: float, price: float) -> str:
    """Show the exact CLI command that would be invoked, without executing it."""
    return (
        f"cd {REPO} && go run ./cmd/order "
        f"{direction.lower()} {pair.lower()} {amount} {price}"
    )


if __name__ == "__main__":
    mcp.run()
