import unittest
from datetime import datetime, timedelta, timezone

from prune_ghcr import plan

NOW = datetime(2026, 10, 13, 12, 0, tzinfo=timezone.utc)
OLD = timedelta(minutes=30)


def ver(i, digest, tags, age_hours):
    t = (NOW - timedelta(hours=age_hours)).strftime("%Y-%m-%dT%H:%M:%SZ")
    return {"id": i, "name": digest, "created_at": t,
            "metadata": {"container": {"tags": tags}}}


def build(n, age_hours, tags):
    """A multi-arch build: index (tagged) + 3 untagged platform manifests."""
    idx = f"sha256:idx{n}"
    kids = [f"sha256:b{n}{p}" for p in ("amd64", "arm64", "armv7")]
    vs = [ver(n * 10, idx, tags, age_hours)] + \
         [ver(n * 10 + 1 + k, d, [], age_hours) for k, d in enumerate(kids)]
    return vs, {idx: kids}


class PlanTest(unittest.TestCase):
    def setUp(self):
        self.versions, self.kids = [], {}
        for n, age in ((1, 50), (2, 40), (3, 30), (4, 5)):  # build 4 is newest
            vs, k = build(n, age, ["latest"] if n == 4 else [f"sha{n}"])
            self.versions += vs
            self.kids.update(k)

    def run_plan(self, keep, min_age=OLD, children=None):
        return plan(self.versions, children or (lambda d: self.kids[d]), keep, NOW, min_age)

    def test_keeps_newest_builds_with_their_platform_manifests(self):
        kept, doomed = self.run_plan(keep=2)
        self.assertEqual([v["name"] for v in kept], ["sha256:idx4", "sha256:idx3"])
        deleted = {v["name"] for v in doomed}
        self.assertEqual(len(doomed), 8)  # builds 1 and 2: index + 3 children each
        for n in (3, 4):
            self.assertNotIn(f"sha256:idx{n}", deleted)
            for p in ("amd64", "arm64", "armv7"):
                self.assertNotIn(f"sha256:b{n}{p}", deleted)

    def test_latest_is_never_deleted_even_with_keep_1(self):
        kept, doomed = self.run_plan(keep=1)
        self.assertEqual(kept[0]["name"], "sha256:idx4")
        self.assertNotIn("sha256:idx4", {v["name"] for v in doomed})

    def test_untagged_children_alone_are_not_garbage_of_a_kept_build(self):
        _, doomed = self.run_plan(keep=4)
        self.assertEqual(doomed, [])

    def test_orphans_are_deleted(self):
        self.versions.append(ver(999, "sha256:orphan", [], 100))
        _, doomed = self.run_plan(keep=4)
        self.assertEqual([v["name"] for v in doomed], ["sha256:orphan"])

    def test_recent_versions_are_left_alone(self):
        self.versions.append(ver(998, "sha256:fresh-orphan", [], 0))  # just pushed
        _, doomed = self.run_plan(keep=4)
        self.assertEqual(doomed, [])

    def test_unreadable_index_aborts_before_any_deletion(self):
        def boom(_digest):
            raise OSError("registry unreachable")
        with self.assertRaises(OSError):
            self.run_plan(keep=2, children=boom)

    def test_refuses_without_tagged_versions(self):
        self.versions = [v for v in self.versions if not v["metadata"]["container"]["tags"]]
        with self.assertRaises(RuntimeError):
            self.run_plan(keep=2)

    def test_keep_must_be_positive(self):
        with self.assertRaises(ValueError):
            self.run_plan(keep=0)


if __name__ == "__main__":
    unittest.main()
