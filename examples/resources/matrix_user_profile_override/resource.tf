data "matrix_whoami" "me" {}

# Bot is a member of #oncall (managed elsewhere) and wants a different
# displayname there than its global one.
#
# Note the depends_on: most homeservers propagate global profile changes to
# all m.room.member events, which wipes per-room overrides if the global
# change happens last. Forcing the override to apply after matrix_user_profile
# avoids perpetual drift.
resource "matrix_user_profile_override" "bot_in_oncall" {
  room_id      = matrix_room.oncall.id
  user_id      = data.matrix_whoami.me.user_id
  display_name = "PagerDuty (on call)"

  depends_on = [matrix_user_profile.bot]
}
