# Composite ID: <room_id>|<event_type>[|<state_key>]
# State key is optional; defaults to "" (empty) for events like m.room.pinned_events.
terraform import matrix_room_state.example '!abcDEF:example.com|m.room.pinned_events'

# With an explicit state_key (e.g. m.room.member keyed by mxid):
terraform import matrix_room_state.other '!abcDEF:example.com|m.room.member|@alice:example.com'
