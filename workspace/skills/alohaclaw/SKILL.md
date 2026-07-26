---
name: alohaclaw
description: Send commands to other bots on the AlohaClaw MQTT network. Supports querying hardware sensors and controlling fans via CTFanBot.
homepage: https://github.com/sipeed/picoclaw
metadata: {"nanobot":{"emoji":"🤖","requires":{"tools":["alohaclaw"]}}}
---

# AlohaClaw Bot Network

Use the `alohaclaw` tool to communicate with other bots on the AlohaClaw MQTT network.

## How It Works

- Each bot has a unique **BotId** (e.g. CTFanBot's BotId, set in its settings)
- `target_id` can be omitted — the tool falls back to the device's configured
  default target (`tools.alohaclaw.default_target_id`); the tool description
  shown to you states what that default currently is. Pass `target_id`
  explicitly only when you need to reach a *different* bot.
- `send_command`: send a text command and wait for the bot's reply
- `send_message`: one-way notification, no reply
- `status`: check if the connection is active

## CTFanBot Commands

```
# Check connection
alohaclaw  action="send_command"  text="Ping"

# List all fan channels (name, RPM, PWM%)
alohaclaw  action="send_command"  text="GetFanChannels"

# List all sensor names and types
alohaclaw  action="send_command"  text="GetSensorNames"

# Get all sensor values at once (temperatures, loads, etc.)
alohaclaw  action="send_command"  text="GetAllSensorValues"

# Get a single sensor value by name or id
alohaclaw  action="send_command"  text="GetSensorValue CPU Core #1"

# Set a fan channel to a specific PWM % (0–100)
alohaclaw  action="send_command"  text="SetFanSpeed CPU Fan 75"
```

(Add `target_id="<other-bot-id>"` to any of the above to target a bot other
than the configured default.)

## Reply Format

Replies are JSON with `success`, `result` (string), and optionally `data` (object):
```json
{ "success": true, "result": "3", "data": [ { "name": "CPU Fan", "rpm": 1200, "pwm_percent": 60 } ] }
```

Always check `success` before reporting results to the user. If `success` is false, `result` contains the error message.

## Typical Workflow

When asked about temperature or fan status:
1. Call `GetAllSensorValues` to get everything at once
2. Filter the relevant sensors from `data` (type: `Temperature`, `Fan`, `Load`)
3. Summarise for the user in natural language

When asked to control a fan:
1. Call `GetFanChannels` first to confirm the channel name
2. Call `SetFanSpeed <channel_name> <pwm%>`

## Notes

- If `send_command` returns a timeout error, CTFanBot may be offline or the BotId may differ
- Use `status` to check the AlohaClaw connection first if commands are failing
- `SetFanSpeed` requires CTFanBot to be running as Administrator
