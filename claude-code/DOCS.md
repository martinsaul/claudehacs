# Claude Code for Home Assistant

Run Claude Code CLI directly from your Home Assistant sidebar.

## Setup

1. Add this repository to your Home Assistant add-on store:
   - Navigate to **Settings > Add-ons > Add-on Store**
   - Click the three-dot menu (top right) > **Repositories**
   - Paste the repository URL and click **Add**

2. Find **Claude Code** in the add-on store and click **Install**

3. Configure the add-on (optional):
   - **Anthropic API Key**: If you have an API key, enter it in the addon configuration. Otherwise, Claude will prompt you to authenticate via OAuth on first launch.
   - **System Prompt**: Set a custom system prompt that is appended to every new Claude session. Useful for giving Claude persistent context about your HA setup or preferred behavior.

4. Start the add-on and click **Open Web UI** or find **Claude** in your sidebar.

## Authentication

Claude Code supports two authentication methods:

- **OAuth (recommended)**: Leave the API key blank. On first launch, Claude will provide a URL to authenticate via your Anthropic account.
- **API Key**: Enter your Anthropic API key in the add-on configuration under Settings.

Authentication persists across add-on restarts.

## Features

- Full Claude Code CLI running in a web terminal
- Accessible from the Home Assistant sidebar
- Direct access to your Home Assistant `/config` directory
- Git, curl, jq, and other common tools pre-installed
- Up to 3 concurrent terminal sessions

## Working Directory

Claude Code starts in `/config`, which is your Home Assistant configuration directory. You can ask Claude to help with:

- Writing and debugging automations
- Editing `configuration.yaml`
- Creating custom scripts and templates
- Reviewing your HA setup
- Any other coding task

## Troubleshooting

**Claude is not in the sidebar**: Make sure the add-on is running. Check that "Show in sidebar" is enabled in the add-on's Info tab.

**Authentication issues**: Stop the add-on, then start it again. If using OAuth, you may need to re-authenticate by following the URL shown in the terminal.

**Terminal not loading**: Check the add-on logs for errors. Ensure your browser allows the connection (some ad blockers may interfere with WebSocket connections).
