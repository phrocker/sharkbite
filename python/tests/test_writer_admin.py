from __future__ import annotations

import ctypes as C
import inspect
import threading
import logging
import unittest
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import patch

from sharkbite import (
    Authorizations,
    BatchWriter,
    Connector,
    LoggingConfiguration,
    Mutation,
    NamespacePermissions,
    SystemPermissions,
    TablePermissions,
)
from sharkbite._native import Bytes, NamespaceView, PropertyView, TableView
from sharkbite.errors import AlreadyExistsError, ClientException, NotFoundError


class Function:
    def __init__(self, callback):
        self.callback = callback

    def __call__(self, *args):
        return self.callback(*args)


def raw(value: Bytes) -> bytes:
    return C.string_at(value.data, value.length) if value.length else b""


class FakeLibrary:
    def __init__(self):
        self.calls = []
        self.frees = []
        self._buffers = []
        self.shoal_connector_config_init = Function(self._init)
        self.shoal_connector_create = Function(self._create)
        self.shoal_connector_close = Function(lambda *_: 0)
        self.shoal_connector_free = Function(lambda *_: self.frees.append("connector"))
        self.shoal_batch_writer_config_init = Function(self._init)
        self.shoal_connector_create_batch_writer = Function(self._writer_create)
        self.shoal_batch_writer_add = Function(self._writer_add)
        self.shoal_batch_writer_flush = Function(lambda *args: self._record("flush", *args))
        self.shoal_batch_writer_size = Function(self._writer_size)
        self.shoal_batch_writer_close = Function(lambda *args: self._record("writer_close", *args))
        self.shoal_batch_writer_free = Function(lambda *_: self.frees.append("writer"))
        self.shoal_mutation_create = Function(self._mutation_create)
        self.shoal_mutation_put = Function(self._mutation_put)
        self.shoal_mutation_put_latest = Function(lambda *args: self._record("put_latest", *args))
        self.shoal_mutation_delete = Function(self._mutation_delete)
        self.shoal_mutation_delete_latest = Function(lambda *args: self._record("delete_latest", *args))
        self.shoal_mutation_size = Function(self._mutation_size)
        self.shoal_mutation_free = Function(lambda *_: self.frees.append("mutation"))
        self.shoal_write_failure_get_flags = Function(lambda *_: 0)
        self.shoal_write_failure_free = Function(lambda *_: self.frees.append("failure"))
        self.shoal_logging_set_level = Function(self._logging_set)
        self.shoal_logging_get_level = Function(lambda: self.log_level)
        self.shoal_logging_set_callback = Function(self._logging_callback)
        self.log_level = 0
        self.mutation_size = 7
        self.log_callback = None
        self._bind_admin()

    @staticmethod
    def _init(pointer):
        C.cast(pointer, C.POINTER(C.c_uint32)).contents.value = C.sizeof(pointer._obj)

    @staticmethod
    def _create(config, out_handle, error):
        C.cast(out_handle, C.POINTER(C.c_void_p)).contents.value = 1
        return 0

    @staticmethod
    def _writer_create(connector, config, out_handle, error):
        C.cast(out_handle, C.POINTER(C.c_void_p)).contents.value = 2
        return 0

    def _record(self, name, *args):
        self.calls.append((name, args))
        return 0

    def _mutation_create(self, row, out_handle, error):
        self.calls.append(("mutation_create", raw(row)))
        C.cast(out_handle, C.POINTER(C.c_void_p)).contents.value = 3
        return 0

    def _mutation_put(self, handle, cf, cq, cv, timestamp, value, error):
        self.calls.append(("put", raw(cf), raw(cq), raw(cv), timestamp, raw(value)))
        return 0

    def _mutation_delete(self, handle, cf, cq, cv, timestamp, error):
        self.calls.append(("delete", raw(cf), raw(cq), raw(cv), timestamp))
        return 0

    def _mutation_size(self, handle, out_size, error):
        C.cast(out_size, C.POINTER(C.c_size_t)).contents.value = self.mutation_size
        return 0

    def _writer_add(self, *args):
        self.calls.append(("add", args[2]))
        return 0

    @staticmethod
    def _writer_size(handle, timeout, out_size, error):
        C.cast(out_size, C.POINTER(C.c_size_t)).contents.value = 3
        return 0

    def _logging_set(self, level, error):
        self.log_level = level
        if self.log_callback:
            self.log_callback(level, b"shoal.logging.level_changed", b'{"level":"debug"}', None)
        return 0

    def _logging_callback(self, callback, context, error):
        self.log_callback = callback if bool(callback) else None
        return 0

    def _bind_admin(self):
        for name in (
            "create_table", "delete_table", "rename_table", "flush_table",
            "flush_table_range", "set_table_property", "remove_table_property",
            "create_namespace", "delete_namespace", "rename_namespace",
            "set_namespace_property", "remove_namespace_property", "create_user",
            "drop_user", "change_password", "change_user_authorizations",
            "grant_system_permission", "revoke_system_permission",
            "grant_table_permission", "revoke_table_permission",
            "grant_namespace_permission", "revoke_namespace_permission",
            "add_table_splits", "remove_table_constraint",
        ):
            setattr(
                self,
                "shoal_connector_" + name,
                Function(lambda *args, _name=name: self._record(_name, *args)),
            )
        self.shoal_connector_table_exists = Function(self._exists)
        self.shoal_connector_namespace_exists = Function(self._exists)
        self.shoal_connector_has_system_permission = Function(self._has)
        self.shoal_connector_has_table_permission = Function(self._has)
        self.shoal_connector_has_namespace_permission = Function(self._has)
        self.shoal_connector_list_tables = Function(self._result)
        self.shoal_table_list_count = Function(lambda *_: 1)
        self.shoal_table_list_get = Function(self._table_get)
        self.shoal_table_list_free = Function(lambda *_: self.frees.append("tables"))
        self.shoal_connector_list_namespaces = Function(self._result)
        self.shoal_namespace_list_count = Function(lambda *_: 1)
        self.shoal_namespace_list_get = Function(self._namespace_get)
        self.shoal_namespace_list_free = Function(lambda *_: self.frees.append("namespaces"))
        for prefix in ("table", "namespace"):
            setattr(self, f"shoal_connector_effective_{prefix}_properties", Function(self._result))
            setattr(self, f"shoal_{prefix}_properties_count", Function(lambda *_: 1))
            setattr(self, f"shoal_{prefix}_properties_get", Function(self._property_get))
            setattr(self, f"shoal_{prefix}_properties_free", Function(lambda *_args, _prefix=prefix: self.frees.append(_prefix + "_properties")))
        self.shoal_connector_namespace_properties = Function(self._result)
        self.shoal_connector_versioned_namespace_properties = Function(self._result)
        self.shoal_versioned_properties_version = Function(lambda *_: 8)
        self.shoal_versioned_properties_count = Function(lambda *_: 1)
        self.shoal_versioned_properties_get = Function(self._property_get)
        self.shoal_versioned_properties_free = Function(lambda *_: self.frees.append("versioned_properties"))
        self.shoal_connector_list_table_splits = Function(self._result)
        self.shoal_connector_get_user_authorizations = Function(self._result)
        self.shoal_bytes_list_count = Function(lambda *_: 1)
        self.shoal_bytes_list_get = Function(self._bytes_get)
        self.shoal_bytes_list_free = Function(lambda *_: self.frees.append("bytes"))
        self.shoal_connector_add_table_constraint = Function(self._add_constraint)
        self.shoal_connector_list_table_constraints = Function(self._result)
        self.shoal_table_constraint_list_count = Function(lambda *_: 0)
        self.shoal_table_constraint_view_init = Function(self._init)
        self.shoal_table_constraint_list_get = Function(lambda *_: 0)
        self.shoal_table_constraint_list_free = Function(lambda *_: None)

    @staticmethod
    def _exists(*args):
        C.cast(args[-2], C.POINTER(C.c_uint8)).contents.value = 1
        return 0

    @staticmethod
    def _has(*args):
        C.cast(args[-2], C.POINTER(C.c_uint8)).contents.value = 1
        return 0

    @staticmethod
    def _result(*args):
        C.cast(args[-2], C.POINTER(C.c_void_p)).contents.value = 99
        return 0

    @staticmethod
    def _table_get(result, index, out_view, error):
        view = C.cast(out_view, C.POINTER(TableView)).contents
        view.name, view.id = b"t", b"1"
        return 0

    @staticmethod
    def _namespace_get(result, index, out_view, error):
        view = C.cast(out_view, C.POINTER(NamespaceView)).contents
        view.name, view.id = b"n", b"2"
        return 0

    @staticmethod
    def _property_get(result, index, out_view, error):
        view = C.cast(out_view, C.POINTER(PropertyView)).contents
        view.key, view.value = b"k", b"v"
        return 0

    def _bytes_get(self, result, index, out_view, error):
        buffer = (C.c_uint8 * 3).from_buffer_copy(b"abc")
        self._buffers.append(buffer)
        view = C.cast(out_view, C.POINTER(Bytes)).contents
        view.data = C.cast(buffer, C.POINTER(C.c_uint8))
        view.length = 3
        return 0

    @staticmethod
    def _add_constraint(connector, table, class_name, timeout, out_number, error):
        C.cast(out_number, C.POINTER(C.c_int32)).contents.value = 4
        return 0


class FakeAPI:
    def __init__(self):
        self.lib = FakeLibrary()
        self.capabilities = frozenset(range(32))

    def require(self, *capabilities):
        if set(capabilities) - self.capabilities:
            raise NotImplementedError

    @staticmethod
    def check(status, error):
        if status:
            raise AssertionError(status)

    def check_write(self, status, failure, error):
        self.check(status, error)


class WriterAdminTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        self.connector = Connector("i", "zk", "u", "p", _api=self.api)

    def tearDown(self):
        self.connector.close()

    def test_mutation_and_writer_copy_inputs_and_close_once(self):
        row = bytearray(b"row")
        with Mutation(row, _api=self.api) as mutation:
            row[:] = b"bad"
            mutation.put(b"cf", b"cq", b"A", 7, b"value")
            self.assertEqual(mutation.size(), 7)
            with BatchWriter(self.connector, "t") as writer:
                self.assertTrue(writer.addMutation(mutation))
                self.assertEqual(writer.size(), 3)
                self.assertTrue(writer.flush())
                writer.close()
        self.assertIn(("mutation_create", b"row"), self.api.lib.calls)
        self.assertIn(("put", b"cf", b"cq", b"A", 7, b"value"), self.api.lib.calls)
        self.assertEqual(self.api.lib.frees.count("writer"), 1)
        self.assertEqual(self.api.lib.frees.count("mutation"), 1)

    def test_process_wide_logging_control_uses_stable_abi(self):
        LoggingConfiguration._set(1, self.api)
        self.assertEqual(self.api.lib.log_level, 1)
        LoggingConfiguration._set(2, self.api)
        self.assertEqual(self.api.lib.log_level, 2)

    def test_writer_accepts_empty_mutation_without_native_rejection(self):
        self.api.lib.mutation_size = 0
        with Mutation(b"row", _api=self.api) as mutation:
            with BatchWriter(self.connector, "t") as writer:
                self.assertTrue(writer.addMutation(mutation))
        self.assertFalse(any(name == "add" for name, _ in self.api.lib.calls))

    def test_writer_context_propagates_body_exception(self):
        with self.assertRaisesRegex(RuntimeError, "body"):
            with BatchWriter(self.connector, "t"):
                raise RuntimeError("body")

    def test_writer_concurrent_adds_are_python_thread_safe(self):
        with Mutation(b"row", _api=self.api) as mutation:
            mutation.put()
            with BatchWriter(self.connector, "t") as writer:
                errors = []

                def add():
                    try:
                        writer.addMutation(mutation)
                    except BaseException as exc:
                        errors.append(exc)

                threads = [threading.Thread(target=add) for _ in range(32)]
                for thread in threads:
                    thread.start()
                for thread in threads:
                    thread.join()
                self.assertFalse(errors)
        self.assertEqual(
            sum(call[0] == "add" for call in self.api.lib.calls),
            32,
        )

    def test_writer_close_waits_for_an_active_add(self):
        started = threading.Event()
        release = threading.Event()
        close_finished = threading.Event()
        original_add = self.api.lib.shoal_batch_writer_add.callback

        def blocked_add(*args):
            started.set()
            release.wait(2)
            return original_add(*args)

        self.api.lib.shoal_batch_writer_add.callback = blocked_add
        with Mutation(b"row", _api=self.api) as mutation:
            mutation.put()
            writer = BatchWriter(self.connector, "t")
            add_thread = threading.Thread(target=writer.addMutation, args=(mutation,))
            close_thread = threading.Thread(
                target=lambda: (writer.close(), close_finished.set())
            )
            add_thread.start()
            self.assertTrue(started.wait(2))
            close_thread.start()
            self.assertFalse(close_finished.wait(0.05))
            self.assertNotIn("writer", self.api.lib.frees)
            release.set()
            add_thread.join(2)
            close_thread.join(2)
            self.assertFalse(add_thread.is_alive())
            self.assertFalse(close_thread.is_alive())
            self.assertTrue(close_finished.is_set())
        self.assertEqual(self.api.lib.frees.count("writer"), 1)

    def test_structured_logging_is_injectable_without_secret_fields(self):
        records = []

        class Handler(logging.Handler):
            def emit(self, record):
                records.append(record)

        logger = logging.Logger("shoal-test")
        logger.addHandler(Handler())
        LoggingConfiguration.configure(logger, api=self.api)
        try:
            LoggingConfiguration._set(1, self.api)
        finally:
            LoggingConfiguration.configure(None, api=self.api)
        self.assertEqual(records[0].msg, "shoal.logging.level_changed")
        self.assertEqual(records[0].shoal, {"level": "debug"})
        self.assertNotIn("password", records[0].shoal)

    def test_mutation_overloads_and_legacy_keyword_names_are_exact(self):
        with self.assertRaises(ClientException):
            Mutation(b"", _api=self.api)
        with Mutation(b"row", _api=self.api) as mutation:
            mutation.put()
            mutation.put(b"cf", b"cq")
            mutation.put(cf=b"f", cq=b"q", cv=b"v", timestamp=11, value=b"x")
            mutation.put(
                column_family=b"family",
                column_qualifier=b"qualifier",
                column_visibility=b"visibility",
            )
            mutation.putDelete(b"df", b"dq", b"dv")
        self.assertIn(("put", b"", b"", b"", 0, b""), self.api.lib.calls)
        self.assertIn(("put", b"cf", b"cq", b"", 0, b""), self.api.lib.calls)
        self.assertIn(("put", b"f", b"q", b"v", 11, b"x"), self.api.lib.calls)
        self.assertIn(
            ("put", b"family", b"qualifier", b"visibility", 0, b""),
            self.api.lib.calls,
        )
        self.assertIn(("delete", b"df", b"dq", b"dv", 0), self.api.lib.calls)

    def test_table_namespace_security_legacy_shapes(self):
        table = self.connector.tableOps("t")
        self.assertTrue(table.exists())
        self.assertEqual(table.getProperties(), {"k": "v"})
        self.assertEqual(table.listSplits(), [b"abc"])
        self.assertEqual(table.addConstraint("Constraint"), 4)
        with self.assertRaisesRegex(NotImplementedError, "online compaction"):
            table.compact(b"", b"", True)

        namespaces = self.connector.namespaceOps("bound")
        self.assertEqual(namespaces.list(), ["n"])
        self.assertEqual(namespaces.list_with_ids(), {"n": "2"})
        self.assertEqual(namespaces.getLocalProperties("n"), {"k": "v"})
        self.assertEqual(namespaces.getVersionedProperties("n"), (8, {"k": "v"}))
        self.assertIsNone(namespaces.create())
        self.assertTrue(namespaces.remove())
        namespaces.rename("new", "old")
        self.assertIn(
            b"old",
            [arg for name, args in self.api.lib.calls if name == "rename_namespace" for arg in args],
        )

        security = self.connector.securityOps()
        self.assertEqual(security.get_auths("u"), Authorizations([b"abc"]))
        self.assertTrue(
            security.has_system_permission("u", SystemPermissions.CREATE_TABLE)
        )
        self.assertTrue(
            security.has_table_permission("u", "t", TablePermissions.WRITE)
        )
        self.assertTrue(
            security.has_namespace_permission("u", "n", NamespacePermissions.READ)
        )
        self.assertEqual(
            security.grant_table_permission("u", "t", TablePermissions.WRITE), 1
        )

    def test_documented_administration_signatures_and_defaults(self):
        table = self.connector.tableOps("t")
        namespaces = self.connector.namespaceOps("bound")
        self.assertEqual(inspect.signature(table.create).parameters["recreate"].default, False)
        self.assertEqual(inspect.signature(table.exists).parameters["createIfNot"].default, False)
        self.assertEqual(inspect.signature(table.compact).parameters["wait"].default, inspect.Parameter.empty)
        self.assertEqual(
            inspect.signature(table.import_directory).parameters["setTime"].default,
            False,
        )
        self.assertEqual(inspect.signature(namespaces.create).parameters["nm"].default, "")
        self.assertEqual(inspect.signature(namespaces.rename).parameters["oldName"].default, "")
        self.assertIs(getattr(table, "import").__func__, table.import_directory.__func__)

    def test_table_lifecycle_returns_and_compatibility_errors(self):
        table = self.connector.tableOps("t")
        with patch.object(table, "_call", side_effect=AlreadyExistsError("exists", status=19)):
            self.assertFalse(table.create())
        with patch.object(table, "_call", side_effect=ClientException("exists", status=19)):
            self.assertFalse(table.create())
        with patch.object(table, "_call", side_effect=NotFoundError("missing", status=9)):
            with self.assertRaises(ClientException) as raised:
                table.remove()
        self.assertEqual(raised.exception.status, 9)
        with patch.object(table, "exists", return_value=True), patch.object(
            table, "remove", return_value=True
        ) as remove, patch.object(table, "_call") as call:
            self.assertTrue(table.create(recreate=True))
            remove.assert_called_once()
            call.assert_called_once()

    def test_property_and_invalid_input_return_contracts(self):
        table = self.connector.tableOps("t")
        before = len(self.api.lib.calls)
        self.assertEqual(table.setProperty("", "value"), -1)
        self.assertEqual(table.removeProperty(""), -1)
        self.assertEqual(len(self.api.lib.calls), before)
        self.assertEqual(table.setProperty("k", "v"), 0)
        self.assertEqual(table.removeProperty("k"), 0)

        security = self.connector.securityOps()
        self.assertEqual(security.create_user("", "p"), -1)
        self.assertEqual(security.change_password("", "p"), -1)
        self.assertEqual(security.remove_user(""), -1)
        self.assertEqual(security.grantAuthorizations(None, "u"), -2)
        self.assertEqual(security.grantAuthorizations([], ""), -1)
        with self.assertRaises(ClientException):
            security.get_auths("")

    def test_namespace_handle_defaults_and_property_order(self):
        namespaces = self.connector.namespaceOps("bound")
        self.assertTrue(namespaces.exists())
        self.assertIsNone(namespaces.setProperty("k", "v"))
        self.assertIsNone(namespaces.removeProperty("k"))
        self.assertIsNone(namespaces.rename("renamed"))
        calls = {name: args for name, args in self.api.lib.calls}
        self.assertIn(b"bound", calls["set_namespace_property"])
        self.assertIn(b"bound", calls["remove_namespace_property"])
        self.assertEqual(calls["rename_namespace"][1:3], (b"bound", b"renamed"))
        self.assertEqual(namespaces.namespace, "renamed")

    def test_permission_enums_are_exact_and_scope_checked(self):
        self.assertEqual(
            {item.name: item.value for item in SystemPermissions},
            {
                "GRANT": 0, "CREATE_TABLE": 1, "DROP_TABLE": 2,
                "ALTER_TABLE": 3, "CREATE_USER": 4, "ALTER_USER": 6,
                "SYSTEM": 7, "CREATE_NAMESPACE": 8, "DROP_NAMESPACE": 9,
                "ALTER_NAMESPACE": 10,
            },
        )
        self.assertEqual(
            {item.name: item.value for item in TablePermissions},
            {
                "READ": 2, "WRITE": 3, "BULK_IMPORT": 4,
                "ALTER_TABLE": 5, "GRANT": 6, "DROP_TABLE": 7,
            },
        )
        self.assertEqual(
            {item.name: item.value for item in NamespacePermissions},
            {
                "READ": 0, "WRITE": 1, "ALTER_NAMESPACE": 2, "GRANT": 3,
                "ALTER_TABLE": 4, "CREATE_TABLE": 5, "DROP_TABLE": 6,
                "BULK_IMPORT": 7, "DROP_NAMESPACE": 8,
            },
        )
        security = self.connector.securityOps()
        with self.assertRaisesRegex(TypeError, "TablePermissions"):
            security.has_table_permission("u", "t", NamespacePermissions.READ)
        with self.assertRaisesRegex(TypeError, "SystemPermissions"):
            security.grant_system_permission("u", TablePermissions.READ)
        with self.assertRaises(ClientException):
            security.has_system_permission("", SystemPermissions.GRANT)

    def test_approved_table_divergences_are_stable(self):
        table = self.connector.tableOps("t")
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-018"):
            table.compact("", "", True)
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-019"):
            getattr(table, "import")("in", "fail")

    def test_fake_administration_calls_are_concurrent(self):
        table = self.connector.tableOps("t")
        with ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(lambda i: table.setProperty(f"k{i}", f"v{i}"), range(64)))
        self.assertEqual(results, [0] * 64)
        property_calls = [call for call in self.api.lib.calls if call[0] == "set_table_property"]
        self.assertEqual(len(property_calls), 64)

    def test_owned_admin_results_are_freed(self):
        table_info = self.connector.tableInfo()
        self.assertEqual(table_info.list(), {"t": "1"})
        self.assertEqual(table_info.list_tables(), ["t"])
        self.assertEqual(table_info.table_id("t"), "1")
        self.assertEqual(table_info.table_name("1"), "t")
        self.assertTrue(table_info.exists("t"))
        self.assertEqual(self.connector.tableOps("t").getProperties(), {"k": "v"})
        self.assertEqual(self.connector.tableOps("t").listSplits(), [b"abc"])
        self.assertIn("tables", self.api.lib.frees)
        self.assertIn("table_properties", self.api.lib.frees)
        self.assertIn("bytes", self.api.lib.frees)


if __name__ == "__main__":
    unittest.main()
