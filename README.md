# Streamserver

## Usage
Running Streamserver as a server in the background will fetch Twitch (and
Strims, if enabled) data every refresh interval (`-r`), and then serve this
data at the specified address (`-a`). The address should match the one set
as the callback URI in your [Twitch project](https://dev.twitch.tv)

You can also set up callbacks for when streams go online or offline using
`srv.SetLiveCallback()` and `srv.SetOfflineCallback()` in `main.go`


## Config
All settings are stored in a config.json file, except for the environment
variable `$BROWSER` which is used to open links in the TUI.

```json
{
    "client_id": "xxx",
    "client_secret": "yyy",
    "user_name": "twitchuser1"
}
```

You can also provide them via the environment variables. You can put this in
a .env file for a Docker deployment. These are then saved internally.

```sh
CLIENT_ID=xxx
CLIENT_SECRET=yyy
USER_NAME=twitchuser1
```

Then run it as follows.

```console
docker build . -t streamserver
docker run --env-file .env --name streamserver -p 8181:8181 streamserver:latest
```

Explanation of environment variables:

`Client ID`: The api key of your Twitch project

`Client Secret`: The secret of your Twitch project

`User ID`: Your Twitch username

## Basic Auth

If the endpoint is deployed publically you can define STREAMS_BASIC_AUTH_PASS
environment variable to require the authenticating client to provide basic
authentication in the form user:pass@domain.com where the user is the same as
the twitch user name. This can be done with streamshower by providing it with
STREAMS_BASIC_AUTH_PASS and STREAMS_BASIC_AUTH_USER.
