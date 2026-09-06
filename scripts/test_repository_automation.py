import importlib.util
from pathlib import Path
import unittest


def load(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


overview = load("upstream-overview")
policy = load("pr-policy")


class AutomationTests(unittest.TestCase):
    def test_no_issue_without_changes(self):
        calls = []
        def api(path, data=None):
            calls.append(path)
            return {"sha": overview.BASELINE} if path.endswith("commits/main") else []
        overview.run("owner/repo", api)
        self.assertEqual(len(calls), 2)

    def test_digest_uses_bot_cursor_and_creates_issue(self):
        head, previous = "a" * 40, "b" * 40
        calls = []
        def api(path, data=None):
            calls.append((path, data))
            if path.endswith("commits/main"):
                return {"sha": head}
            if "issues?" in path:
                return [{"user": {"login": "stranger"}, "body": f"<!-- upstream-head:{head} -->"},
                        {"user": {"login": "github-actions[bot]"}, "body": f"<!-- upstream-head:{previous} -->"}]
            if "/compare/" in path:
                self.assertIn(previous + "..." + head, path)
                return {"commits": [{"sha": head, "commit": {"message": "Fix @someone <markup>\nMore"}}]}
            self.assertIn(f"<!-- upstream-head:{head} -->", data["body"])
            self.assertNotIn("@someone", data["body"])
            self.assertEqual(data["labels"], ["upstream"])
            return {"html_url": "https://example.com/issue"}
        overview.run("owner/repo", api)
        self.assertEqual(len(calls), 4)

    def pr(self, title="feat: new behavior", body="Refs #12", base="staging", head="feat/example"):
        return {"title": title, "body": body, "base": {"ref": base},
                "head": {"ref": head, "repo": {"full_name": "owner/repo"}}}

    def test_feature_needs_real_issue(self):
        policy.validate(self.pr(), "owner/repo", lambda n: {"number": int(n)})
        for body in ("", "Refs #12"):
            with self.assertRaises(ValueError):
                policy.validate(self.pr(body=body), "owner/repo", lambda n: {"pull_request": {}})

    def test_docs_and_dependencies_exempt(self):
        for title in ("docs: clarify setup", "build(deps): bump dependency"):
            policy.validate(self.pr(title=title, body=""), "owner/repo", lambda n: self.fail("Unexpected issue lookup"))

    def test_promotion_routing(self):
        policy.validate(self.pr(base="main", head="staging"), "owner/repo", lambda n: {})
        with self.assertRaises(ValueError):
            policy.validate(self.pr(base="main"), "owner/repo", lambda n: {})


if __name__ == "__main__":
    unittest.main()
