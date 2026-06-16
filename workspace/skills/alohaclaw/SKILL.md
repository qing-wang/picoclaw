---
name: alohaclaw
description: Send commands and messages to other bots on the AlohaClaw MQTT network, such as querying hardware status or controlling devices via CTFanBot.
homepage: https://github.com/sipeed/picoclaw
metadata: {"nanobot":{"emoji":"🤖","requires":{"tools":["alohaclaw"]}}}
---

# AlohaClaw Bot Network

Use the `alohaclaw` tool to communicate with other bots on the AlohaClaw MQTT network.

## How It Works

- Each bot has a unique **BotId** (e.g. `CTFanBot`)
- `send_command`: send a request and wait for the bot's reply — use for queries or actions that return a result
- `send_message`: one-way notification, no reply expected
- `status`: check if the AlohaClaw connection is active

## Common Usage

```
# Query CTFanBot for CPU temperatures
alohaclaw  action="send_command"  target_id="CTFanBot"  text="get_temperatures"

# Request current fan status
alohaclaw  action="send_command"  target_id="CTFanBot"  text="get_fan_status"

# Set fan mode (fire-and-forget)
alohaclaw  action="send_command"  target_id="CTFanBot"  text="set_fan_mode silent"  wait_reply=false

# Send a notification
alohaclaw  action="send_message"  target_id="CTFanBot"  text="hello from PicoClaw"

# Check connection
alohaclaw  action="status"
```

## Reply Format

CTFanBot replies in JSON with `success`, `result`, and optionally `data` fields:
```json
{ "success": true, "result": "temperatures retrieved", "data": { "cpu": 72.5 } }
```

Always check `success` before reporting results to the user.

## Notes

- Commands are delivered over TLS-secured MQTT — no setup needed beyond config
- If `send_command` returns a timeout error, the target bot may be offline or the command may be unrecognised
- Use `status` first if commands are failing unexpectedly
