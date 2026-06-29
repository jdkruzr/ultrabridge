# Tasks, CalDAV, And Attachments

UltraBridge stores tasks in SQLite and exposes them through the web UI, JSON API, MCP tools, CalDAV, and supported device sync surfaces.

## CalDAV URLs

Use discovery when your client supports it:

```text
https://your-host/.well-known/caldav
```

Direct collection URL:

```text
https://your-host/caldav/tasks/
```

Use your UltraBridge username/password unless a client explicitly supports bearer tokens.

## Supported Task Fields

UltraBridge maps the practical task fields used across its integrations:

- Title
- Description/detail/comment
- Status and completion time
- Due date
- Priority
- Categories
- URL/native links
- Signed attachments

Recurrence, reminders, and other advanced client-specific fields are not fully modeled.

## Attachments

Task attachments are stored internally and exposed as signed URLs from UltraBridge. This is used for ForestNote-rendered task context and generic CalDAV attachment flows.

If a client does not show attachments:

1. Check the task in UltraBridge or MCP output.
2. Fetch the CalDAV object and confirm it contains an `ATTACH` URI.
3. Fetch the attachment URL and confirm it returns `200`.
4. Remember that some task clients ignore `ATTACH` even when calendars support it.

## MCP Task Tools

MCP task mutations use the same service layer as the API and web UI. Created, updated, completed, deleted, and purged tasks sync through the configured downstream surfaces.

Every mutation is audit-logged with the auth method and label where available.
