"""Pin failure cases that would otherwise allow broken documentation."""

from pathlib import Path
import tempfile
import unittest

from check_docs import check


class DocsTests(unittest.TestCase):
    """Local links and guide navigation must fail when their targets drift."""

    def test_rejects_missing_heading(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / 'README.md').write_text('# Start\n\n[broken](#missing)\n')
            self.assertTrue(any('missing heading' in error for error in check(root)))

    def test_rejects_duplicate_page_order(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            group = root / 'docs/guides/start'
            group.mkdir(parents=True)
            for name in ('README', 'one', 'two'):
                (group / f'{name}.md').write_text(f'---\ntitle: {name}\ndescription: Test\norder: 1\n---\n\n# {name}\n')
            self.assertTrue(any('order duplicates' in error for error in check(root)))
