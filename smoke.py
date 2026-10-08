"""Live end-to-end check: starts the server over stdio and calls real tools."""

import asyncio
import sys
from pathlib import Path

from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

HERE = Path(__file__).parent


async def main() -> None:
    params = StdioServerParameters(
        command="uv", args=["run", "--directory", str(HERE), "python", "server.py"]
    )
    async with stdio_client(params) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            tools = await session.list_tools()
            print("tools:", [t.name for t in tools.tools])

            stats = await session.call_tool("hardcover_stats", {})
            print("\nhardcover_stats:", stats.content[0].text)

            books = await session.call_tool("hardcover_library", {"status": "read", "limit": 3})
            print("\nhardcover_library(read, 3):", books.content[0].text)

            search = await session.call_tool("hardcover_search", {"query": "Мастер и Маргарита"})
            print("\nhardcover_search:", search.content[0].text)


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit(1)
