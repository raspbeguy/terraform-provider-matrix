# Warning: a bad ACL cannot be undone. If it blocks your own server, every
# remote server rejects this room's events from you, and rejects a corrective
# ACL too. Your own server still accepts your events, so local users see no
# change while federation stays broken for good.
#
# allow must contain your homeserver or "*". An empty or absent allow list
# denies every server, so this provider refuses that plan.
resource "matrix_room_server_acl" "example" {
  room_id           = matrix_room.example.id
  allow             = ["*"]
  deny              = ["evil.example.com", "*.spam.example"]
  allow_ip_literals = false
}
