Hook payload fixtures.

These are written from the Claude Code hooks documentation
(https://code.claude.com/docs/en/hooks), not captured from a live session.
The documented fields are session_id, transcript_path, cwd, hook_event_name,
permission_mode plus: SessionStart.source, UserPromptSubmit.prompt,
PostToolUse.tool_name/tool_input/tool_response, PostToolUseFailure.error,
SessionEnd.reason. The Stop and SessionEnd input schemas were not in the
part of the page that was read; re-capture those against a real session.
