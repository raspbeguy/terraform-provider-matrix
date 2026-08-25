# Import by space ID. A space is a room whose creation_content.type is m.space,
# so the ID looks the same as a room's. The provider checks that type and
# refuses to import a plain room as a matrix_space.
terraform import matrix_space.example '!abcDEF:example.com'

# The import reads back what the homeserver reports: room_version, visibility
# and room_alias_name. Two attributes have no endpoint that reports them, so
# they stay null in state: preset and initial_invites. If your configuration
# sets either, the first plan after the import shows an in-place update. That
# apply changes nothing on the homeserver. It records the declared values, and
# every later plan is clean.
