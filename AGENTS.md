# AGENTS.md — working on DokWalt

Instructions for coding agents (and humans) changing this repository. Read `README.md` for
what DokWalt is, how to install it and how to build it from source.

## Layout

| Path                     | What it is                                                                          |
| ------------------------ | ----------------------------------------------------------------------------------- |
| `cmd/dokwalt/`           | Entry point: one binary, the CLI on your machine and the daemon on the server       |
| `internal/`              | All the code (`cli`, `daemon`, `engine`, `compose`, `proxy`, `store`, `backup`, …)  |
| `internal/docs/topics/`  | User documentation: embedded in the binary (`dokwalt docs`) and source of the website docs |
| `internal/docs/sitegen/` | Generator of the website docs (`site/docs/`) from the topics                        |
| `site/`                  | Landing page and docs website: static, served by Caddy, deployed with DokWalt itself |
| `docs/SPEC.md`           | Technical specification, for maintainers                                            |
| `test/e2e/`              | End-to-end suite against a throwaway systemd + Docker server container              |

## Commands

| Task                                    | Command                                       |
| --------------------------------------- | --------------------------------------------- |
| Build the CLI and server binaries       | `make build`                                  |
| Unit tests (must pass before you finish) | `make test`                                  |
| Vet and formatting                      | `make lint`                                   |
| End-to-end suite                        | `make e2e`                                    |
| Regenerate the website docs             | `make docs-site`                              |
| Deploy the website (only when asked)    | `dokwalt deploy` from `site/`                 |

## Documentation (always in sync with the code)

Documentation is part of every change, never a follow-up. Each fact lives in one place.

| Document                 | Contains                                                                         |
| ------------------------ | -------------------------------------------------------------------------------- |
| `README.md`              | Overview, requirements, install, first deploy, everyday commands, development   |
| `internal/docs/topics/`  | The user documentation: what every command and feature does. One source for `dokwalt docs` and the website |
| `site/docs/`             | Generated from the topics: never edit the HTML by hand. `docs.css` and `docs.js` are hand-written |
| `internal/docs/sitegen/diagrams/` | Schemas as SVG, never ASCII art on the website. A topic keeps a text version for the terminal in a ` ```text diagram=<name> ` block; the website shows `<name>.svg` instead |
| `docs/SPEC.md`           | How DokWalt is built. When it disagrees with the code, the code wins and the spec gets fixed |
| `site/index.html`        | The landing page: features, version, commands shown                              |

## Definition of done

A change is done when:

1. `make lint` and `make test` pass.
2. New behavior has tests. Run `make e2e` too when the change touches what it covers: deploys,
   config releases, rollbacks, pipelines, the database tunnel, reboot recovery.
3. Docs are in sync: the topics in `internal/docs/topics/` (and `--help` texts, the README and
   `docs/SPEC.md` where relevant) describe the new behavior, and no document describes the old one.
4. **The docs website is up to date.** `make docs-site` regenerated `site/docs/` from the topics:
   `make test` fails while it is stale. A new topic is added to a group in
   `internal/docs/sitegen/main.go` (generation fails otherwise). The website documents only the
   latest release: after tagging a release, run `make docs-site` again so the pages show that
   version, update the version on the landing page, and redeploy the site.
5. Nothing is deployed, pushed, tagged or published unless the user asked for it.
