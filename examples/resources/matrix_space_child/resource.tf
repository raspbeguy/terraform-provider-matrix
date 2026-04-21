resource "matrix_space_child" "example" {
  parent_space_id = matrix_space.engineering.id
  child_room_id   = matrix_room.backend.id
  suggested       = true
  order           = "01"
  via             = ["example.com"]
}
