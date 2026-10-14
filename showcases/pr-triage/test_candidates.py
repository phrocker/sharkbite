# Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
import unittest
from candidates import change_text, render


class CandidateTest(unittest.TestCase):
    def unit(self, before, after):
        return {"path": "auth.go", "symbol": "function:Check", "before": {"text": before}, "after": {"text": after}}

    def test_diff_preserves_removed_check_and_added_return(self):
        unit = self.unit("func Check() {\n if !allowed() { deny() }\n}\n", "func Check() {\n return\n}\n")
        patch = change_text(unit)
        self.assertIn("- if !allowed() { deny() }", patch)
        self.assertIn("+ return", patch)
        state = render(unit, "diff")
        self.assertFalse(state["context_complete"])
        self.assertIn("unchanged body omitted", state["view"])

    def test_unknown_or_missing_context_does_not_silently_render(self):
        unit = self.unit("before", "after")
        for representation in ("invented", "diff_context"):
            with self.assertRaises(ValueError):
                render(unit, representation)

    def test_original_representation_retains_exact_bytes(self):
        unit = self.unit("// original\nfunc Check() {}\n", "// changed\nfunc Check() {}\n")
        state = render(unit, "original")
        self.assertEqual(state["before"], unit["before"]["text"])
        self.assertEqual(state["after"], unit["after"]["text"])


if __name__ == "__main__":
    unittest.main()
