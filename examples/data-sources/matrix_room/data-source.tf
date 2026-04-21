data "matrix_room" "support" {
  alias = "#support:example.com"
}

output "support_room_id" {
  value = data.matrix_room.support.room_id
}
