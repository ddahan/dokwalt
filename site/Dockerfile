# The landing page, served by Caddy. Deploy it with DokWalt itself:
#   cd site && dokwalt apps:create dokwalt-site && dokwalt deploy
FROM caddy:2-alpine
COPY . /srv
CMD ["caddy", "file-server", "--root", "/srv", "--listen", ":8080"]
