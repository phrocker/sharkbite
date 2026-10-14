import unittest

import poisoning
import verify_poisoning


class PoisoningCorpusTests(unittest.TestCase):
    def test_versioned_corpus_has_expected_attack_denominators(self):
        report = poisoning.run()
        self.assertEqual(report['corpus_id'], poisoning.CORPUS_ID)
        self.assertEqual(report['corpus_version'], poisoning.CORPUS_VERSION)
        self.assertEqual(report['denominators']['attacks'], len(poisoning.ATTACKS))
        self.assertEqual(report['denominators']['rejected'], 7)
        self.assertEqual(report['denominators']['quarantined'], 2)
        self.assertEqual(report['denominators']['unsafe'], 0)
        self.assertEqual(report['denominators']['failed'], 0)
        self.assertEqual(report['clean_control']['status'], 'accepted')

    def test_receipt_verifier_accepts_report_and_rejects_tampering(self):
        report = poisoning.run()
        verified = verify_poisoning.verify(report)
        self.assertTrue(verified['verified'])
        tampered = dict(report)
        tampered['denominators'] = dict(report['denominators'])
        tampered['denominators']['unsafe'] = 1
        with self.assertRaises(ValueError):
            verify_poisoning.verify(tampered)
        tampered = dict(report)
        tampered['schema'] = 2
        with self.assertRaises(ValueError):
            verify_poisoning.verify(tampered)


if __name__ == '__main__':
    unittest.main()
