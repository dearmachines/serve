# Skills

This directory contains LLM-facing instructions for working on Serve.

Use `skills/serve/SKILL.md` with coding agents that support skill files. If your agent does not have a skill system, paste the contents of that file into the agent's system/developer instructions before asking it to modify this repository.

Host provisioning is outside Serve's scope: Docker, the Serve binary, the systemd unit, required directories, SOPS, and credentials are installed and configured manually.

Serve configuration uses `env.plain` for non-sensitive environment values and `env.secret` for names resolved from the SOPS-encrypted `serve.secrets.yml`. Both application and accessory containers support this schema.
