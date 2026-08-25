# Import by room ID (starts with !, ends with :homeserver). The provider checks
# creation_content.type and refuses to import a space as a matrix_room.
terraform import matrix_room.example '!abcDEF:example.com'

# The import reads back what the homeserver reports: room_version, visibility,
# room_alias_name and encryption_enabled. Three attributes have no endpoint that
# reports them, so they stay null in state: preset, is_direct and
# initial_invites. If your configuration sets any of them, the first plan after
# the import shows an in-place update. That apply changes nothing on the
# homeserver. It records the declared values, and every later plan is clean.
