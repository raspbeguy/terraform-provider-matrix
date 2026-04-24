resource "matrix_room" "example" {
  name               = "#general"
  topic              = "Team chat"
  preset             = "private_chat"
  visibility         = "private"
  history_visibility = "shared"
  room_alias_name    = "team-general"
  initial_invites    = ["@alice:example.com", "@bob:example.com"]
}
