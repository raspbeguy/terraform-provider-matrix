# via is required by the Matrix specification: without it a client cannot resolve
# the child room. It also matters here because removing a link is done by writing
# empty content, so a link with no via, order or suggested looks removed and
# disappears from state on the next refresh. The provider warns at plan time.
#
# Leave order or suggested out and the link keeps whatever the space already
# holds, rather than clearing it.
resource "matrix_space_child" "example" {
  parent_space_id = matrix_space.engineering.id
  child_room_id   = matrix_room.backend.id
  suggested       = true
  order           = "01"
  via             = ["example.com"]
}
