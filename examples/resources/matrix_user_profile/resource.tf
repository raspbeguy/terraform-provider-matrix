# Manage the bot account's global displayname and avatar declaratively.
# Useful when the same bot is deployed across dev/staging/prod and you want
# the visible identity to track config.
resource "matrix_user_profile" "bot" {
  display_name = "PagerDuty Bot"
  avatar_url   = "mxc://example.com/abcDEFghi123"
}

output "bot_user_id" {
  value = matrix_user_profile.bot.id
}
