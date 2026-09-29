# Creates the test account `alice` and an API token for the e2e (#3234).
# 何度走っても同じ状態になるようにする (compose を up し直しても壊れない)。
user = User.find_by(email: 'alice@example.com')
unless user
  account = Account.new(username: 'alice')
  user = User.new(
    email: 'alice@example.com',
    password: SecureRandom.hex(16),
    agreement: true,
    approved: true,
    confirmed_at: Time.now.utc,
    account: account,
  )
  user.save!(validate: false)
end
# 登録が承認制の既定だと、作った直後は承認待ちになる。
user.update_columns(approved: true, confirmed_at: user.confirmed_at || Time.now.utc)

app = Doorkeeper::Application.find_or_create_by!(name: 'mk-go e2e') do |a|
  a.redirect_uri = 'urn:ietf:wg:oauth:2.0:oob'
  a.scopes = 'read write follow'
end
token = Doorkeeper::AccessToken.find_or_create_for(
  application: app,
  resource_owner: user,
  scopes: Doorkeeper::OAuth::Scopes.from_string('read write follow'),
)
File.write('/shared/mastodon_token', token.token)
puts "created token for #{user.account.username}"
