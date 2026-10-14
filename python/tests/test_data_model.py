from __future__ import annotations

import unittest

from sharkbite import (
    Authorizations,
    IterInfo,
    Key,
    KeyValue,
    PythonIterator,
    Range,
    Value,
)


class DataModelTests(unittest.TestCase):
    def test_value_null_binary_copy_text_comparison_hash_and_rendering(self):
        empty = Value()
        self.assertEqual(str(empty), "")
        self.assertEqual(repr(empty), "[]")
        source = bytearray(b"a\x00\xff")
        value = Value(source)
        source[:] = b"changed"
        self.assertEqual(value.get_bytes(), b"a\x00\xff")
        self.assertEqual(value.get().encode("utf-8", "surrogateescape"), b"a\x00\xff")
        self.assertEqual(value, Value(b"a\x00\xff"))
        self.assertEqual(hash(value), hash(Value(b"a\x00\xff")))

    def test_key_defaults_mutation_binary_access_order_hash_and_string(self):
        key = Key()
        self.assertEqual(key.getTimestamp(), (1 << 63) - 1)
        self.assertEqual(str(key), " : []")
        key = Key(b"")
        self.assertEqual(str(key), " : [] 9223372036854775807")
        self.assertIs(key.setRow(b"r\x00"), key)
        key.setColumnFamily("cf").setColumnQualifier("cq")
        self.assertEqual(key.getRow().encode("utf-8", "surrogateescape"), b"r\x00")
        self.assertEqual(key.get_row_bytes(), b"r\x00")
        self.assertLess(Key(b"r", timestamp=2), Key(b"r", timestamp=1))
        self.assertEqual(hash(key), hash(key.copy()))

    def test_key_value_owns_copies_and_default_holders(self):
        key = Key(b"row")
        value = Value(b"value")
        pair = KeyValue(key, value)
        key.setRow(b"changed")
        value.set(b"changed")
        self.assertEqual(pair.getKey().get_row_bytes(), b"row")
        self.assertEqual(pair.getValue().get_bytes(), b"value")
        default = KeyValue()
        self.assertEqual(str(default.getKey()), " : []")
        self.assertEqual(repr(default.getValue()), "[]")

    def test_range_overloads_bounds_aliases_and_predicates(self):
        infinite = Range()
        self.assertTrue(infinite.inifinite_start_key())
        self.assertTrue(infinite.infinite_stop_key())
        row = Range(b"row")
        self.assertTrue(row.start_key_inclusive())
        self.assertTrue(row.stop_key_inclusive())
        self.assertEqual(row.get_start_key().get_row_bytes(), b"row")
        half = Range(Key(b"a"), False)
        self.assertFalse(half.start_key_inclusive())
        self.assertTrue(half.inifinite_stop_key())
        bounded = Range(Key(b"a"), True, Key(b"z"), False)
        self.assertTrue(bounded.before_start_key(Key(b"0")))
        self.assertTrue(bounded.after_end_key(Key(b"z")))
        self.assertEqual(Range("", True, None, False).__str__(), "Range (-inf,+inf) ")
        updated_row = Range(b"a", True, b"z", True, True)
        self.assertEqual(updated_row.get_stop_key().get_row_bytes(), b"z\x00")
        self.assertFalse(updated_row.stop_key_inclusive())
        updated_key = Range(Key(b"a"), True, Key(b"z"), True, True)
        self.assertEqual(updated_key.get_stop_key().get_row_bytes(), b"z\x00")
        self.assertTrue(updated_key.stop_key_inclusive())

    def test_authorizations_are_sorted_deduplicated_mutable_and_binary_safe(self):
        auths = Authorizations([b"z", b"a", b"z", b"\xff"])
        self.assertEqual(auths.getAuthorizations(), [b"a", b"z", b"\xff"])
        self.assertTrue(auths.contains(b"\xff"))
        auths.addAuthorization(b"b")
        self.assertEqual(list(auths), [b"a", b"b", b"z", b"\xff"])
        self.assertEqual(str(Authorizations()), "[ ]")
        self.assertEqual(hash(auths), hash(Authorizations(list(auths))))

    def test_iterator_descriptors_publish_all_aliases(self):
        info = IterInfo("age", "example.AgeFilter", 10)
        self.assertEqual(info.getPriority(), info.priority())
        self.assertEqual(info.getName(), info.name())
        self.assertEqual(info.getClass(), getattr(info, "class")())
        script_info = IterInfo(
            "print('script')", "python-script", 11, type="Python"
        )
        self.assertEqual(script_info.getName(), "python-script")
        self.assertEqual(script_info.getClass(), "org.poma.accumulo.JythonIterator")
        iterator = PythonIterator("python", 7)
        self.assertIs(iterator.onNext("lambda key, value: True"), iterator)
        self.assertEqual(iterator.script, "lambda key, value: True")
        self.assertEqual(iterator.getClass(), "org.poma.accumulo.JythonIterator")
        with self.assertRaises(RuntimeError):
            PythonIterator("python", "class python: pass", 7).onNext(
                "lambda value: value"
            )


if __name__ == "__main__":
    unittest.main()
