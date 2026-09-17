#!/usr/bin/env bash
# Bump ODH_COMPONENT_MANIFESTS branch@sha pins in opt/manifest-sources.sh to the
# current HEAD of each tracking branch (for example stable). RHOAI pins are left
# unchanged. Used by .github/workflows/manifests-sync-stable.yaml and for local refresh:
#   bash ci/bump-odh-manifest-shas.sh && make manifests-fetch
#
# Exits non-zero when no ODH branch@sha pins are present so unpinned branch
# heads are not fetched by accident.
set -euo pipefail

_script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_FILE="${1:-${_script_dir}/../opt/manifest-sources.sh}"

if [[ ! -f "${SCRIPT_FILE}" ]]; then
    echo "ERROR: ${SCRIPT_FILE} not found" >&2
    exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
    echo "ERROR: python3 is required" >&2
    exit 1
fi

if ! command -v git >/dev/null 2>&1; then
    echo "ERROR: git is required" >&2
    exit 1
fi

# GH_TOKEN / GITHUB_TOKEN avoid unauthenticated git ls-remote rate limits in CI.
export GH_TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"

python3 - "${SCRIPT_FILE}" <<'PY'
import os
import re
import subprocess
import sys
from pathlib import Path

script_path = Path(sys.argv[1])
text = script_path.read_text()
match = re.search(r"declare -A ODH_COMPONENT_MANIFESTS=\((.*?)\)", text, re.S)
if match is None:
    raise SystemExit("ODH_COMPONENT_MANIFESTS block not found")

pin_re = re.compile(
    r"(?P<org>[A-Za-z0-9._-]+):(?P<repo>[A-Za-z0-9._-]+):"
    r"(?P<branch>[A-Za-z0-9._/-]+)@(?P<sha>[0-9a-f]{7,40}):"
)
safe_ref = re.compile(r"^[A-Za-z0-9._/-]+$")
token = os.environ.get("GH_TOKEN", "")
resolved: dict[tuple[str, str, str], str] = {}
updated = False

if not pin_re.search(match.group(1)):
    raise SystemExit(
        "no ODH branch@sha pins found in "
        f"{script_path}; refusing to fetch unpinned branch heads"
    )


def head_sha(org: str, repo: str, branch: str) -> str:
    key = (org, repo, branch)
    if key in resolved:
        return resolved[key]
    if not (safe_ref.match(org) and safe_ref.match(repo) and safe_ref.match(branch)):
        raise SystemExit(f"invalid git ref components: {org}/{repo} {branch}")
    url = f"https://github.com/{org}/{repo}.git"
    cmd = ["git", "ls-remote", "--heads", url, f"refs/heads/{branch}"]
    if token:
        cmd = ["git", "-c", f"http.extraheader=AUTHORIZATION: bearer {token}", *cmd[1:]]
    print(f"Resolving latest SHA for {org}/{repo} branch {branch}...")
    out = subprocess.check_output(cmd, text=True).strip()
    sha = out.split()[0] if out else ""
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise SystemExit(f"failed to resolve SHA for {org}/{repo} refs/heads/{branch}")
    resolved[key] = sha
    return sha


def replace_pin(m: re.Match[str]) -> str:
    global updated
    org, repo, branch, old_sha = m.group("org", "repo", "branch", "sha")
    new_sha = head_sha(org, repo, branch)
    if new_sha == old_sha:
        print(f"  {org}/{repo}:{branch} already at {new_sha}")
    else:
        print(f"  {org}/{repo}:{branch} {old_sha} -> {new_sha}")
        updated = True
    return f"{org}:{repo}:{branch}@{new_sha}:"


new_block = pin_re.sub(replace_pin, match.group(1))
script_path.write_text(text[: match.start(1)] + new_block + text[match.end(1) :])
if updated:
    print(f"Updated ODH tracking SHAs in {script_path}")
else:
    print(f"ODH tracking SHAs in {script_path} are already current")
PY
