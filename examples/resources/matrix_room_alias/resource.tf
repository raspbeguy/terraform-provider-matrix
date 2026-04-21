resource "matrix_room_alias" "example" {
  alias   = "#team-general-alt:example.com"
  room_id = matrix_room.example.id
}
