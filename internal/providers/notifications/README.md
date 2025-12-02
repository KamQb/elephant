### Elephant Notifications

Store and search desktop notification history.

#### Features

- Captures notifications via freedesktop DBus interface
- Configurable history limit
- Persistent storage (optional)
- Search through notification history
- Dismiss individual or all notifications
- Copy notification content to clipboard

#### Requirements

- DBus session bus
- `wl-clipboard` (for copy action)

#### Configuration

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `max_items` | int | 100 | Maximum number of notifications to keep |
| `persist` | bool | true | Persist notifications across restarts |

#### Actions

| Action | Description |
|--------|-------------|
| `dismiss` | Remove notification from history |
| `dismiss_all` | Clear all notifications |
| `copy` | Copy summary and body to clipboard |
| `copy_body` | Copy only body to clipboard |

#### Notes

This provider can act as a notification daemon or listen passively:
- **Primary mode**: If no other notification daemon is running, elephant becomes the notification server
- **Passive mode**: If another daemon is active, elephant listens for notification signals

To use elephant as your notification daemon, ensure no other notification daemon (like `dunst`, `mako`, etc.) is running.
