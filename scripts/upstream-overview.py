#!/usr/bin/env python3
"""Create an idempotent, commit-linked upstream digest without importing code."""
import json
import os
import re
import subprocess

UPSTREAM = "owainlewis/machinist"
BASELINE = "fda16c207da88c7dac32aac6473839fb2b1906b8"


def api(endpoint, data=None):
    command = ["gh", "api", endpoint]
    if data is not None:
        command += ["--method", "POST", "--input", "-"]
    result = subprocess.run(command, input=json.dumps(data) if data is not None else None,
                            text=True, capture_output=True, check=True)
    return json.loads(result.stdout)


def overview_body(base, head, commits):
    lines = [f"<!-- upstream-head:{head} -->", "## Upstream changes", "",
             f"[Compare the complete range](https://github.com/{UPSTREAM}/compare/{base}...{head})",
             "", "Choose individual improvements for this fork. No code has been imported.", "",
             "## Commits", ""]
    for commit in commits[:100]:
        sha = commit["sha"]
        # Titles are untrusted: strip mentions/markup, retain the stable commit link.
        title = re.sub(r"[^\w .,():/+-]", "", commit["commit"]["message"].splitlines()[0])[:200]
        lines.append(f"- [ ] [{sha[:7]}](https://github.com/{UPSTREAM}/commit/{sha}) — {title}")
    if len(commits) >= 100:
        lines += ["", "The list is capped at 100 commits; use the complete comparison above."]
    lines += ["", "## Selection", "", "Record selected commits and follow-up issue/PR links here."]
    return "\n".join(lines)


def run(repo, request=api):
    head = request(f"repos/{UPSTREAM}/commits/main")["sha"]
    base = BASELINE
    # Newest-created first: subsequent comments must not change the cursor.
    issues = request(f"repos/{repo}/issues?state=all&labels=upstream&sort=created&direction=desc&per_page=100")
    for issue in issues:
        if issue.get("user", {}).get("login") != "github-actions[bot]" or "pull_request" in issue:
            continue
        match = re.search(r"<!-- upstream-head:([0-9a-f]{40}) -->", issue.get("body") or "")
        if match:
            base = match[1]
            break
    if base == head:
        print("No new upstream commits; no issue created.")
        return
    comparison = request(f"repos/{UPSTREAM}/compare/{base}...{head}?per_page=100")
    body = overview_body(base, head, comparison["commits"])
    if comparison.get("status") in ("diverged", "behind"):
        body += "\n\nUpstream history changed; inspect the comparison before selecting commits.\n"
    issue = request(f"repos/{repo}/issues", {
        "title": f"Upstream overview: {base[:7]} → {head[:7]}",
        "body": body, "labels": ["upstream"], "assignees": ["CalvinDittkrist"]})
    print(issue["html_url"])


if __name__ == "__main__":
    run(os.environ["GITHUB_REPOSITORY"])
