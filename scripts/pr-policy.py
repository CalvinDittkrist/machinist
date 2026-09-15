#!/usr/bin/env python3
"""Validate contribution routing and issue references using GitHub PR metadata."""
import json
import os
import re
import subprocess


def validate(pr, repo, issue_lookup):
    if pr["base"]["ref"] != "main":
        raise ValueError("Open pull requests against main.")
    author = pr.get("user", {})
    if author.get("login") == "dependabot[bot]" and author.get("type") == "Bot":
        return
    match = re.match(r"^(feat|fix|docs|chore|build|ci|refactor|test|perf|revert|deps)(\([^\n)]+\))?!?: .+", pr["title"])
    if not match:
        raise ValueError("Use a conventional PR title, for example feat: add a feature.")
    if match[1] not in ("feat", "fix"):
        return
    numbers = re.findall(r"(?im)^\s*(?:refs|fixes|closes|resolves)\s+#(\d+)\b", pr.get("body") or "")
    for number in numbers:
        issue = issue_lookup(number)
        if "pull_request" not in issue:
            return
    raise ValueError("Features and fixes need a real repository issue: add Refs #123 to the PR body.")


if __name__ == "__main__":
    with open(os.environ["GITHUB_EVENT_PATH"]) as stream:
        event = json.load(stream)
    repo = os.environ["GITHUB_REPOSITORY"]

    def lookup(number):
        return json.loads(subprocess.check_output(["gh", "api", f"repos/{repo}/issues/{number}"], text=True))

    validate(event["pull_request"], repo, lookup)
    print("PR policy passed.")
