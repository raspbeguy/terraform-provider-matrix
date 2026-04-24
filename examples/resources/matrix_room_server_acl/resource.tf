# Warning: a misconfigured ACL can irreversibly lock your homeserver out of
# the room. If allow is non-empty, make sure it matches your own homeserver
# (literally or via "*"); if deny matches your homeserver, you lose access.
# Recovery requires a homeserver admin. See the resource docs for details.
resource "matrix_room_server_acl" "example" {
  room_id           = matrix_room.example.id
  allow             = ["*"]
  deny              = ["evil.example.com", "*.spam.example"]
  allow_ip_literals = false
}
