# Claude Code for Home Assistant

A Home Assistant add-on that puts [Claude Code](https://docs.anthropic.com/en/docs/claude-code) directly in your sidebar.

[![Open your Home Assistant instance and show the add add-on repository dialog with a specific repository URL pre-filled.](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fmartinsaul%2Fclaudehacs)

## What is this?

This add-on bundles the Claude Code CLI into a web terminal accessible from your Home Assistant sidebar. Click "Claude" in the sidebar, and you get a full Claude Code session with direct access to your HA configuration directory.

## Installation

1. Add this repository to your Home Assistant instance:
   - Click the button above, **or**
   - Go to **Settings > Add-ons > Add-on Store > Repositories** and add this repo's URL

2. Install the **Claude Code** add-on

3. (Optional) Configure your Anthropic API key, or use OAuth

4. Start the add-on — "Claude" appears in your sidebar

## Features

- Full Claude Code CLI in a web terminal
- Sidebar integration — one click to open
- Starts in `/config` so Claude can work on your HA setup
- OAuth and API key authentication
- Git, curl, jq, and other tools included
- Supports amd64 and aarch64

## Configuration

| Option | Description |
|--------|-------------|
| `anthropic_api_key` | Your Anthropic API key (optional — leave blank for OAuth) |

## License

MIT
