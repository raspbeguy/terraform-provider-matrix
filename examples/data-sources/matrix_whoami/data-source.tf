data "matrix_whoami" "me" {}

output "my_user_id" {
  value = data.matrix_whoami.me.user_id
}
