resource "matrix_room_member" "invite_bob" {
  room_id    = matrix_room.example.id
  user_id    = "@bob:example.com"
  membership = "invite"
}
