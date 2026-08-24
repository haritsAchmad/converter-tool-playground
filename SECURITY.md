# Security Policy

## Reporting

Please do not open a public issue for a suspected vulnerability. Use GitHub's private vulnerability reporting feature for this repository and include reproduction steps, affected versions, and impact. Do not include real user files or secrets.

## Scope and expectations

The current release is an MVP intended for self-hosting. Maintainers will acknowledge a report when practical, validate it, prepare a fix, and coordinate disclosure. No bug bounty is currently offered.

Run the published container profile or equivalent isolation. Do not mount secrets, the Docker socket, broad host directories, or cloud credentials into the worker. Apply HTTPS and rate limiting at the reverse proxy before exposing it publicly.
