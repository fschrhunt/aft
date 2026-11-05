"""Pin release selection and formula manifest failure cases."""

import unittest

from release import release_notes
from formula import formula, TARGETS


class ReleaseNotesTests(unittest.TestCase):
    """Release notes must identify exactly one stable version section."""

    def test_extracts_only_selected_section(self):
        notes = '# Changelog\n\n## Unreleased\n\nLater\n\n## 1.2.3\n\n### Fixes\n\n- Fixed.\n\n## 1.2.2\n\nEarlier\n'
        self.assertEqual(release_notes('v1.2.3', notes), '### Fixes\n\n- Fixed.\n')

    def test_rejects_unsafe_or_ambiguous_tags(self):
        for tag in ('../escape', 'v01.2.3', '1.2.3', 'v1.2.3-rc1', 'v1.2.3\n'):
            with self.subTest(tag=tag), self.assertRaises(ValueError):
                release_notes(tag, '## 1.2.3\nNotes\n')

    def test_requires_release_notes(self):
        with self.assertRaises(ValueError):
            release_notes('v1.2.3', '## Unreleased\nPending\n')

    def test_rejects_duplicate_version_sections(self):
        with self.assertRaises(ValueError):
            release_notes('v1.2.3', '## 1.2.3\nFirst\n## 1.2.3\nSecond\n')


class FormulaTests(unittest.TestCase):
    """A formula must use exactly the release's checksums for every platform."""

    def manifest(self):
        """Return distinct platform digests to catch swapped architecture mappings."""
        return ''.join(f'{str(i) * 64}  aft-v0.1.0-{target}.tar.gz\n'
                       for i, target in enumerate(TARGETS, 1))

    def test_platforms_match_the_release_manifest(self):
        result = formula('v0.1.0', self.manifest())
        for i, target in enumerate(TARGETS, 1):
            self.assertIn(f'url "https://github.com/fschrhunt/aft/releases/download/v0.1.0/aft-v0.1.0-{target}.tar.gz"\n      sha256 "{str(i) * 64}"', result)
        self.assertIn('assert_equal "aft v0.1.0"', result)
        self.assertIn('bin.install "aft"', result)

    def test_invalid_manifests_do_not_generate_a_formula(self):
        valid = self.manifest()
        for manifest in ('', valid.splitlines()[0], valid + valid.splitlines()[0],
                         valid.replace('1' * 64, 'not-a-checksum'),
                         valid.replace('linux-arm64', 'windows-arm64')):
            with self.subTest(manifest=manifest), self.assertRaises(ValueError):
                formula('v0.1.0', manifest)
        with self.assertRaises(ValueError):
            formula('v0.1.0;anything', valid)
