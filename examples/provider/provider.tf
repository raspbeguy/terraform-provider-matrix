terraform {
  required_providers {
    matrix = {
      source  = "raspbeguy/matrix"
      version = "~> 0.1"
    }
  }
}

provider "matrix" {
  homeserver_url = "https://matrix.example.com"
  # access_token = "syt_…"   # prefer MATRIX_ACCESS_TOKEN env var
  # user_id      = "@tf:matrix.example.com"
}
