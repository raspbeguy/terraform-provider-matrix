# The import ID must equal the caller's mxid (the user authenticated by the
# provider's access_token). Any other value is rejected.
terraform import matrix_user_profile.bot '@bot:example.com'
