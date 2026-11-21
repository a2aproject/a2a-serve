# Changelog

## [0.2.0](https://github.com/a2aproject/a2a-serve/compare/v0.1.0...v0.2.0) (2026-11-21)


### Features

* add a2a-run --contact for other A2A agents the agent may call ([#599](https://github.com/a2aproject/a2a-serve/issues/599)) ([5012e26](https://github.com/a2aproject/a2a-serve/commit/5012e26690608710abd63aa19b467c851cb85472))
* add a2a-run --egress-proxy, letting a host proxy decide each sandbox connection ([#594](https://github.com/a2aproject/a2a-serve/issues/594)) ([27a61a2](https://github.com/a2aproject/a2a-serve/commit/27a61a2b37c8a084464f3b015d1b748e8b760292))
* add a2a-run --tools to serve the host's MCP tools and hooks ([#597](https://github.com/a2aproject/a2a-serve/issues/597)) ([f391dde](https://github.com/a2aproject/a2a-serve/commit/f391dde623aaf2d611b276773d0372a024c93936))
* add a2a-run, a sidecar that serves a sandboxed coding agent over A2A ([#590](https://github.com/a2aproject/a2a-serve/issues/590)) ([d7d5e10](https://github.com/a2aproject/a2a-serve/commit/d7d5e10641f8fde4fe220863e3ed490bb78c9174))
* add memory and turn-synced workspaces to a2a-run ([#598](https://github.com/a2aproject/a2a-serve/issues/598)) ([2953ae5](https://github.com/a2aproject/a2a-serve/commit/2953ae57c57e33c6ccf90df413b283f7ed499cc5))
* add the network provider protocol, its Go SDK, host and conformance suite ([#500](https://github.com/a2aproject/a2a-serve/issues/500)) ([a9d8ce1](https://github.com/a2aproject/a2a-serve/commit/a9d8ce17fcd06ccf7f8cac62bae70db653653984))
* add the SLIM network provider ([#503](https://github.com/a2aproject/a2a-serve/issues/503)) ([6e9bd85](https://github.com/a2aproject/a2a-serve/commit/6e9bd85493ec7c23b6256d591ca1220435b7f20d))
* **core:** add a bubblewrap sandbox runtime ([#604](https://github.com/a2aproject/a2a-serve/issues/604)) ([1e8d2ca](https://github.com/a2aproject/a2a-serve/commit/1e8d2cab447d7dc8e0d383146083c204e8dbb056))
* **core:** add egress.Router to decide each sandbox connection in code ([#592](https://github.com/a2aproject/a2a-serve/issues/592)) ([0e34e45](https://github.com/a2aproject/a2a-serve/commit/0e34e45b450fb2337dd7ae516591ed1d70229869))
* **core:** add live workspaces served over FUSE ([#600](https://github.com/a2aproject/a2a-serve/issues/600)) ([d651cff](https://github.com/a2aproject/a2a-serve/commit/d651cffcdce9c29e77e8f87dcfe2555718e3c246))
* **core:** add workspace sources and turn-synced workspaces ([#593](https://github.com/a2aproject/a2a-serve/issues/593)) ([a7de3bb](https://github.com/a2aproject/a2a-serve/commit/a7de3bbe09cb09793fe5c7d61d27c5842261b7ca))
* find, download and install network providers by fingerprint ([#501](https://github.com/a2aproject/a2a-serve/issues/501)) ([62ff755](https://github.com/a2aproject/a2a-serve/commit/62ff7557be6eba6569e9ddea213e9bce4a364d91))
* run a SLIM relay with `relay run --backend slim` ([#497](https://github.com/a2aproject/a2a-serve/issues/497)) ([8feadde](https://github.com/a2aproject/a2a-serve/commit/8feadde8bbad7ae6ef151683ce80d4c8448decde))
* run networks in network providers, and move Agent Mesh to its own module ([#502](https://github.com/a2aproject/a2a-serve/issues/502)) ([fd17a52](https://github.com/a2aproject/a2a-serve/commit/fd17a522962a6f7a6f23e2ae87d2d77b5ed79a8e))
* **sandbox:** live workspaces on rootful Docker with a narrow AppArmor profile ([#616](https://github.com/a2aproject/a2a-serve/issues/616)) ([7b5bc46](https://github.com/a2aproject/a2a-serve/commit/7b5bc4645d4ecdb10a3fdc3a2a5b73bb7889c7c6)), closes [#610](https://github.com/a2aproject/a2a-serve/issues/610)
* **sandbox:** run Docker sandboxes under gVisor ([#617](https://github.com/a2aproject/a2a-serve/issues/617)) ([061731c](https://github.com/a2aproject/a2a-serve/commit/061731cb9a7636ca7e5c3baab58b400d1823c502)), closes [#610](https://github.com/a2aproject/a2a-serve/issues/610)
* **sandbox:** run the agent with bubblewrap in a2a-serve ([#618](https://github.com/a2aproject/a2a-serve/issues/618)) ([f2cfde4](https://github.com/a2aproject/a2a-serve/commit/f2cfde473f6e3af8797de397272226336a85291c)), closes [#610](https://github.com/a2aproject/a2a-serve/issues/610)
* serve a2a-run workspaces from the host, live or synced at turns ([#603](https://github.com/a2aproject/a2a-serve/issues/603)) ([6e49cc1](https://github.com/a2aproject/a2a-serve/commit/6e49cc1e4d943fcfd6f59462dc3416cc09670707))
* **sidecar:** make the host conformance suite a public package ([#615](https://github.com/a2aproject/a2a-serve/issues/615)) ([18d60a8](https://github.com/a2aproject/a2a-serve/commit/18d60a8f73b0476dacdf791c3607540ec0ede56e)), closes [#610](https://github.com/a2aproject/a2a-serve/issues/610)


### Bug Fixes

* a2a-run issues found by user simulation ([#607](https://github.com/a2aproject/a2a-serve/issues/607)) ([60a33e8](https://github.com/a2aproject/a2a-serve/commit/60a33e8475c3e817e7fb47acf262ddf216bb61c7))
* **a2a-run:** leave .git directories out of the changes artifact ([#611](https://github.com/a2aproject/a2a-serve/issues/611)) ([f5c3ed5](https://github.com/a2aproject/a2a-serve/commit/f5c3ed515530562bd52fa16308726fab3bb86375))
* **chat:** show that the server stopped a turn after a slow end ([#630](https://github.com/a2aproject/a2a-serve/issues/630)) ([9b48868](https://github.com/a2aproject/a2a-serve/commit/9b48868276358f5ab69543a6122327d9cdc4eaeb))
* CLI hints and output from the user simulation at 1b67edf ([#545](https://github.com/a2aproject/a2a-serve/issues/545)) ([b52acf5](https://github.com/a2aproject/a2a-serve/commit/b52acf5d79e14bb14d98e7be766fbb6800137204)), closes [#530](https://github.com/a2aproject/a2a-serve/issues/530)
* concurrent detached starts and a cancel race in the synced workspace tests ([#605](https://github.com/a2aproject/a2a-serve/issues/605)) ([5e41f15](https://github.com/a2aproject/a2a-serve/commit/5e41f15df179795555a0f4e899aba851ad53fff4))
* **core:** create the parents of the Docker env file directory ([#591](https://github.com/a2aproject/a2a-serve/issues/591)) ([17d1d39](https://github.com/a2aproject/a2a-serve/commit/17d1d39afc6d191d19f2fb09f89a3dfe18fcacaf))
* **core:** don't count identical writes as turn-sync conflicts or changes ([#606](https://github.com/a2aproject/a2a-serve/issues/606)) ([6f0cb70](https://github.com/a2aproject/a2a-serve/commit/6f0cb7097f5455ff723dffa760632f91fb3b4a53))
* **core:** retry a cancel that lost a race with the end of its execution ([#602](https://github.com/a2aproject/a2a-serve/issues/602)) ([235151a](https://github.com/a2aproject/a2a-serve/commit/235151a8d5b843cdd633756782eb8f2d53597367))
* **egress:** don't log a block for connections the boundary closes as it stops ([#628](https://github.com/a2aproject/a2a-serve/issues/628)) ([b55f044](https://github.com/a2aproject/a2a-serve/commit/b55f044dcd76de1198cb53b79fc8505609579f26))
* end unread responses after a timeout, and cap each member's open requests and streams ([#548](https://github.com/a2aproject/a2a-serve/issues/548)) ([5a76aa1](https://github.com/a2aproject/a2a-serve/commit/5a76aa16d23de65e1282e04ba60c8ac2e9f8652e)), closes [#521](https://github.com/a2aproject/a2a-serve/issues/521) [#522](https://github.com/a2aproject/a2a-serve/issues/522)
* **examples:** inline the SLIM node config, so example 07 starts from a clone other users can't read ([#537](https://github.com/a2aproject/a2a-serve/issues/537)) ([43a8f25](https://github.com/a2aproject/a2a-serve/commit/43a8f252df164eae915067de6d749fd1b7a1b369)), closes [#529](https://github.com/a2aproject/a2a-serve/issues/529)
* fail a message queued behind a turn that stops for approval, naming the command that answers it ([#549](https://github.com/a2aproject/a2a-serve/issues/549)) ([c5030d6](https://github.com/a2aproject/a2a-serve/commit/c5030d6cb60f9a5cb215cfd5a5b2a68ad8c3fc45)), closes [#528](https://github.com/a2aproject/a2a-serve/issues/528)
* find a peer that started right after the agent, without waiting a minute ([#543](https://github.com/a2aproject/a2a-serve/issues/543)) ([dc82134](https://github.com/a2aproject/a2a-serve/commit/dc821343f9d04eb8cfc3c33a796ca0b6072a5f42)), closes [#519](https://github.com/a2aproject/a2a-serve/issues/519)
* **fusefs:** don't bring back removed files or write renamed ones at the old name ([#624](https://github.com/a2aproject/a2a-serve/issues/624)) ([4fc5c2d](https://github.com/a2aproject/a2a-serve/commit/4fc5c2df56d193f6f4bd59bd55a650b028438a91))
* give downloads of the binaries their own limit, so stalled downloads leave room for pairing, sign-in and approvals ([#566](https://github.com/a2aproject/a2a-serve/issues/566)) ([246db77](https://github.com/a2aproject/a2a-serve/commit/246db77013d5903acaca65aa6412a83712c203cc)), closes [#558](https://github.com/a2aproject/a2a-serve/issues/558)
* install a network provider from a second path without breaking the first ([#538](https://github.com/a2aproject/a2a-serve/issues/538)) ([bb5b66d](https://github.com/a2aproject/a2a-serve/commit/bb5b66d115db47b63be7277bf6b3307f68b0ebe3)), closes [#524](https://github.com/a2aproject/a2a-serve/issues/524)
* keep a chat message that arrives while the server stops ([1df84cd](https://github.com/a2aproject/a2a-serve/commit/1df84cd82155b08cf9567277275421010b84482f))
* keep a tunneled request's body open while the response streams, so a peer's stream isn't cut ([#516](https://github.com/a2aproject/a2a-serve/issues/516)) ([6f86b15](https://github.com/a2aproject/a2a-serve/commit/6f86b15d8eb9411149feaba0f3a987602c42c39b))
* leave a network of a non-default provider with config unset and --private none ([#541](https://github.com/a2aproject/a2a-serve/issues/541)) ([3c64a04](https://github.com/a2aproject/a2a-serve/commit/3c64a04dfb80a8afde4b9df8f06877d331b3d5e8)), closes [#518](https://github.com/a2aproject/a2a-serve/issues/518)
* let restore replace unused state, take backup now through the running agent, and keep interrupted restores' secrets hidden ([#573](https://github.com/a2aproject/a2a-serve/issues/573)) ([ce2b5f1](https://github.com/a2aproject/a2a-serve/commit/ce2b5f18d069b13531b13191a25b382afd72dc96)), closes [#554](https://github.com/a2aproject/a2a-serve/issues/554) [#561](https://github.com/a2aproject/a2a-serve/issues/561)
* log why members' tasks fail, and refuse invites, tool rules and messages that would mislead the owner ([#547](https://github.com/a2aproject/a2a-serve/issues/547)) ([75156ed](https://github.com/a2aproject/a2a-serve/commit/75156edd0ff6de0635528b724ba467043ddfbe01))
* make commands and hints from the review of cb010cd work as printed ([#569](https://github.com/a2aproject/a2a-serve/issues/569)) ([194b748](https://github.com/a2aproject/a2a-serve/commit/194b7485e45d16d8321ce4743d467ecb7645872b)), closes [#563](https://github.com/a2aproject/a2a-serve/issues/563)
* pair invite prints a join command people can run when the private URL works only from agents ([#542](https://github.com/a2aproject/a2a-serve/issues/542)) ([e785a5d](https://github.com/a2aproject/a2a-serve/commit/e785a5d84c5822c6682e1c946ff738161e3442b1)), closes [#525](https://github.com/a2aproject/a2a-serve/issues/525)
* quiet retries of refused one-time schedules, keep the end of long approvals when answered, and remove schedules that post to a removed member ([#568](https://github.com/a2aproject/a2a-serve/issues/568)) ([bb932b9](https://github.com/a2aproject/a2a-serve/commit/bb932b9fe9d5ce0a4d52ec40c8bb1cf47bcea5fd)), closes [#562](https://github.com/a2aproject/a2a-serve/issues/562)
* refuse null parts, and count data parts, extensions and task references against the message limits ([#544](https://github.com/a2aproject/a2a-serve/issues/544)) ([e6560f4](https://github.com/a2aproject/a2a-serve/commit/e6560f4a2a1e23086046531c28aeaa146a9ec3eb)), closes [#520](https://github.com/a2aproject/a2a-serve/issues/520) [#523](https://github.com/a2aproject/a2a-serve/issues/523)
* report a network provider that stops answering, and fail calls through it in bounded time ([#546](https://github.com/a2aproject/a2a-serve/issues/546)) ([5a9f379](https://github.com/a2aproject/a2a-serve/commit/5a9f3792f7b1551bdc635c74204cfd187f807356)), closes [#526](https://github.com/a2aproject/a2a-serve/issues/526)
* report a running server whose control socket is gone, and stop the server that stop asked to stop ([#572](https://github.com/a2aproject/a2a-serve/issues/572)) ([e6e0746](https://github.com/a2aproject/a2a-serve/commit/e6e0746e3330ea9e6961362748cca16927253dd3)), closes [#560](https://github.com/a2aproject/a2a-serve/issues/560) [#565](https://github.com/a2aproject/a2a-serve/issues/565)
* retry starting a network provider whose binary is still busy ([#510](https://github.com/a2aproject/a2a-serve/issues/510)) ([b0c01cf](https://github.com/a2aproject/a2a-serve/commit/b0c01cf578fd705f953203b7efd5d8324ac240b8))
* revoke a pairing whose agent home has read-only directories ([#567](https://github.com/a2aproject/a2a-serve/issues/567)) ([824e52d](https://github.com/a2aproject/a2a-serve/commit/824e52d3a7e46407947279d8461f249ffe8ff945)), closes [#555](https://github.com/a2aproject/a2a-serve/issues/555)
* **sandbox:** don't fail the bubblewrap check while other mounts change ([#619](https://github.com/a2aproject/a2a-serve/issues/619)) ([326931c](https://github.com/a2aproject/a2a-serve/commit/326931cf47316a59f29d5c4c0e25c5d423328a90))
* save config changes that add no problem, and delete secrets with the settings that use them ([#570](https://github.com/a2aproject/a2a-serve/issues/570)) ([ec21c4a](https://github.com/a2aproject/a2a-serve/commit/ec21c4a75d6515831941cb79c48aa48e14d52666)), closes [#559](https://github.com/a2aproject/a2a-serve/issues/559) [#564](https://github.com/a2aproject/a2a-serve/issues/564)
* show the stop message in chats on a busy machine, and fix test flakes under load ([#586](https://github.com/a2aproject/a2a-serve/issues/586)) ([1322a84](https://github.com/a2aproject/a2a-serve/commit/1322a843d6a714268d55d4fb70fae324c7547693))
* skip the first-run run question when start has --detach or --service ([#539](https://github.com/a2aproject/a2a-serve/issues/539)) ([6082306](https://github.com/a2aproject/a2a-serve/commit/60823061363d66a32bda8a6d0ae24fce6f659b67)), closes [#527](https://github.com/a2aproject/a2a-serve/issues/527)
* **slim:** join the node again after it restarts ([#612](https://github.com/a2aproject/a2a-serve/issues/612)) ([539c934](https://github.com/a2aproject/a2a-serve/commit/539c93466baae2f4ba837fa3167a5e9045d67507))
* **start:** pick the local port below the ephemeral range ([#625](https://github.com/a2aproject/a2a-serve/issues/625)) ([18feb11](https://github.com/a2aproject/a2a-serve/commit/18feb11c8c555d4fc6af393b1595403640ad6160))
* **start:** print the whole startup summary before returning to the shell ([#623](https://github.com/a2aproject/a2a-serve/issues/623)) ([6da54df](https://github.com/a2aproject/a2a-serve/commit/6da54df760a6605ece768e3c175e7f26e1e22549))
* tell chat users how a restart ended their turn, and queue new Discord threads on their own ([#571](https://github.com/a2aproject/a2a-serve/issues/571)) ([d695cc3](https://github.com/a2aproject/a2a-serve/commit/d695cc3d2b54eba7180489386a9d55a71133664f)), closes [#556](https://github.com/a2aproject/a2a-serve/issues/556) [#557](https://github.com/a2aproject/a2a-serve/issues/557)
* wait for a server that is starting or stopping, and say when a stop ended a starting server ([#553](https://github.com/a2aproject/a2a-serve/issues/553)) ([368f11c](https://github.com/a2aproject/a2a-serve/commit/368f11c79d93b650fd9130961d783dfdefe1af4f)), closes [#550](https://github.com/a2aproject/a2a-serve/issues/550)
* warn about tool entries that name an MCP server without a tool, and document owner posts in members' conversations ([#551](https://github.com/a2aproject/a2a-serve/issues/551)) ([cb010cd](https://github.com/a2aproject/a2a-serve/commit/cb010cd1f89932b3c50dfc3332ee026a7ce50dc9)), closes [#531](https://github.com/a2aproject/a2a-serve/issues/531)
* warn about tool entries that name an MCP server without a tool, and document owner posts in members' conversations ([#551](https://github.com/a2aproject/a2a-serve/issues/551)) ([59cb951](https://github.com/a2aproject/a2a-serve/commit/59cb9514f5c6221d7200accab4c0a6164aee0017)), closes [#531](https://github.com/a2aproject/a2a-serve/issues/531)

## 0.1.0 (2026-11-11)

First release. A2A Serve turns opencode, Claude Code or any ACP coding agent into a sandboxed [A2A](https://a2a-protocol.org) agent that people, messengers and other agents can call. See the [documentation](https://a2aproject.github.io/a2a-serve/).

### Features

#### Setup and sandbox

* **[Supported agents](https://a2aproject.github.io/a2a-serve/guide/supported-agents/):** run opencode, Claude Code or any other ACP agent on Linux and macOS.
* **[Running](https://a2aproject.github.io/a2a-serve/guide/running/):** run one agent per project directory, in the foreground, detached or as a user service.
* **[Config](https://a2aproject.github.io/a2a-serve/guide/config/):** keep settings in `.a2a/config.yaml` and change them with `a2a-serve config set` or `config edit`.
* **[Secrets](https://a2aproject.github.io/a2a-serve/guide/secrets/):** store API keys and tokens, and inject model keys and headers so they stay out of the sandbox.
* **[Sandbox](https://a2aproject.github.io/a2a-serve/guide/sandbox/):** run the agent in Docker, with separate containers for members and guests, and storage, secret and egress rules.
* **`a2a-serve doctor`:** check the setup and fix common problems with `--fix`.

#### Extend and operate

* **[MCP and skills](https://a2aproject.github.io/a2a-serve/guide/mcp-and-skills/):** give the agent the MCP servers and skills of its project.
* **[Recipes](https://a2aproject.github.io/a2a-serve/guide/recipes/):** create a ready-made agent from a recipe directory with one command.
* **[Schedules](https://a2aproject.github.io/a2a-serve/guide/schedules/):** run prompts on a cron schedule or at a set time, and send results to chat.
* **[Backups](https://a2aproject.github.io/a2a-serve/guide/backups/):** take encrypted snapshots of an agent and restore it on another machine.
* **[Shell completion](https://a2aproject.github.io/a2a-serve/guide/shell-completion/):** completion scripts for bash, zsh, fish and PowerShell.

#### Access and approvals

* **[Approvals](https://a2aproject.github.io/a2a-serve/guide/approvals/):** approve edits and commands from an A2A client, chat or the inspector, and build rules from "Always allow".
* **[Inspector](https://a2aproject.github.io/a2a-serve/guide/inspector/):** a web page with conversations, tasks, pending approvals and status.
* **[Identities](https://a2aproject.github.io/a2a-serve/guide/identities/):** owner, member and guest roles, with pairing codes for new callers.
* **[Email sign-in](https://a2aproject.github.io/a2a-serve/guide/email-sign-in/):** let people on an email allowlist call the agent with Google sign-in.

#### Networks and chat

* **[Networks](https://a2aproject.github.io/a2a-serve/guide/networks/):** expose the agent on Tailscale, an Agent Mesh relay or a public URL.
* **[Relays](https://a2aproject.github.io/a2a-serve/guide/relays/):** run an Agent Mesh relay that agents join from anywhere, and manage its members.
* **[Messengers](https://a2aproject.github.io/a2a-serve/guide/messengers/):** talk to the agent through Telegram, Discord and email.
* **[Voice](https://a2aproject.github.io/a2a-serve/guide/voice/):** send voice notes and get voice replies through OpenAI, Gemini or your own speech server.

#### Multi-agent

* **[Agent2Agent](https://a2aproject.github.io/a2a-serve/guide/agent2agent/):** let the agent call contacts and peers, with the server holding the credentials.
* **[Webhook nodes](https://a2aproject.github.io/a2a-serve/guide/webhook-nodes/):** give agents that run only when woken an A2A address, and wake them with a signed POST.
