from __future__ import annotations

import ctypes as C
import copy
import unittest

import pysharkbite
import sharkbite
from sharkbite import (
    AccumuloInfo,
    AuthInfo,
    Client,
    Configuration,
    Connector,
    Key,
    PythonIterator,
    ScannerOptions,
    TabletServerStatus,
    ZookeeperInstance,
)
from sharkbite._native import (
    CAP_HIGH_LEVEL_CLIENT,
    CAPABILITY_SYMBOLS,
    Bytes,
    ClientConfig,
    ConnectorConfig,
    ConnectorIdentityView,
    KeyValueView,
    NativeAPI,
    Range,
)
from sharkbite.errors import (
    CLIENT_ERROR_CODES,
    ClientException,
    InvalidArgumentError,
    InvalidHandleError,
    exception_for_status,
)


class Function:
    def __init__(self, callback):
        self.callback = callback
        self.restype = None
        self.argtypes = None

    def __call__(self, *args):
        return self.callback(*args)


class FakeLibrary:
    def __init__(self):
        self.closed = []
        self.freed = []
        self.result_freed = 0
        self.authorizations = []
        self.identity_freed = 0
        self.resolve_timeouts = []
        self.connector_timeouts = []
        self.connector_create_status = 0
        self._buffers = []
        self.shoal_connector_config_init = Function(self._init)
        self.shoal_client_config_init = Function(self._init)
        self.shoal_range_init = Function(self._init)
        self.shoal_connector_create = Function(self._create)
        self.shoal_connector_close = Function(lambda handle, error: self._close("connector"))
        self.shoal_connector_free = Function(lambda handle: self._free("connector"))
        self.shoal_client_create = Function(self._create)
        self.shoal_client_close = Function(lambda handle, error: self._close("client"))
        self.shoal_client_free = Function(lambda handle: self._free("client"))
        self.shoal_client_set_threads = Function(lambda *args: 0)
        self.shoal_client_set_table = Function(lambda *args: 0)
        self.shoal_client_select_column = Function(lambda *args: 0)
        self.shoal_client_scan_range = Function(self._scan)
        self.shoal_scan_result_count = Function(lambda result: 1)
        self.shoal_scan_result_get = Function(self._get)
        self.shoal_scan_result_free = Function(self._result_free)
        self.shoal_zookeeper_resolve_instance = Function(self._resolve_instance)
        self.shoal_connector_identity_view_init = Function(self._init)
        self.shoal_connector_identity_get = Function(self._identity_get)
        self.shoal_connector_identity_free = Function(self._identity_free)

    @staticmethod
    def _init(pointer):
        C.cast(pointer, C.POINTER(C.c_uint32)).contents.value = C.sizeof(
            pointer._obj
        )

    def _create(self, config, out_handle, error):
        if isinstance(config._obj, ConnectorConfig):
            connector = C.cast(config, C.POINTER(ConnectorConfig)).contents
            self.connector_timeouts.append(
                connector.zookeeper_session_timeout_ms
            )
        elif isinstance(config._obj, ClientConfig):
            native = C.cast(config, C.POINTER(ClientConfig)).contents
            self.authorizations = [
                C.string_at(native.authorizations[index].data, native.authorizations[index].length)
                for index in range(native.authorization_count)
            ]
        C.cast(out_handle, C.POINTER(C.c_void_p)).contents.value = 123
        return self.connector_create_status

    def _resolve_instance(
        self, instance, zookeepers, session_timeout, bootstrap_timeout,
        secret, out_result, error
    ):
        del zookeepers, bootstrap_timeout, secret, error
        self.resolve_timeouts.append(session_timeout)
        self._resolved_name = instance
        self._resolved_id = b"uuid-1"
        C.cast(out_result, C.POINTER(C.c_void_p)).contents.value = 789
        return 0

    def _identity_get(self, result, out_view, error):
        del result, error
        view = C.cast(out_view, C.POINTER(ConnectorIdentityView)).contents
        view.instance_name = self._resolved_name
        view.instance_id = self._resolved_id
        view.principal = b""
        return 0

    def _identity_free(self, result):
        self.identity_freed += 1
        C.cast(result, C.POINTER(C.c_void_p)).contents.value = None

    def _close(self, kind):
        self.closed.append(kind)
        return 0

    def _free(self, kind):
        self.freed.append(kind)

    @staticmethod
    def _scan(handle, scan_range, timeout, out_result, error):
        C.cast(out_result, C.POINTER(C.c_void_p)).contents.value = 456
        return 0

    def _value(self, value: bytes) -> Bytes:
        buffer = (C.c_uint8 * len(value)).from_buffer_copy(value)
        self._buffers.append(buffer)
        return Bytes(C.cast(buffer, C.POINTER(C.c_uint8)), len(value))

    def _get(self, result, index, out_view, error):
        view = C.cast(out_view, C.POINTER(KeyValueView)).contents
        view.row = self._value(b"row")
        view.column_family = self._value(b"cf")
        view.column_qualifier = self._value(b"cq")
        view.column_visibility = self._value(b"A")
        view.timestamp = 7
        view.value = self._value(b"value")
        return 0

    def _result_free(self, result):
        self.result_freed += 1


class FakeAPI:
    def __init__(self):
        self.lib = FakeLibrary()
        self.capabilities = frozenset(range(32))

    def require(self, *capabilities):
        missing = set(capabilities) - self.capabilities
        if missing:
            raise NotImplementedError

    @staticmethod
    def check(status, error):
        if status:
            raise AssertionError(status)

    @staticmethod
    def copy_view(view):
        def read(value):
            return C.string_at(value.data, value.length) if value.length else b""

        return (
            (
                read(view.row),
                read(view.column_family),
                read(view.column_qualifier),
                read(view.column_visibility),
                view.timestamp,
            ),
            read(view.value),
        )


class FoundationTests(unittest.TestCase):
    def test_import_aliases(self):
        self.assertIs(pysharkbite.Client, sharkbite.Client)
        self.assertIs(pysharkbite.Key, sharkbite.Key)

    def test_connector_context_close_is_idempotent(self):
        api = FakeAPI()
        with Connector("i", "zk", "u", "p", _api=api) as connector:
            self.assertFalse(connector.closed)
        connector.close()
        self.assertEqual(api.lib.closed, ["connector"])
        self.assertEqual(api.lib.freed, ["connector"])
        self.assertEqual(api.lib.connector_timeouts, [1000])

    def test_configuration_instance_credentials_and_explicit_connector(self):
        api = FakeAPI()
        configuration = Configuration()
        configuration.set("client.key", "before")
        with ZookeeperInstance(
            "i", "zk1:2181,zk2:2181", 0, configuration, _api=api
        ) as instance:
            self.assertIsInstance(instance, sharkbite.Instance)
            self.assertEqual(instance.getInstanceName(), "i")
            self.assertEqual(instance.instance_name(), "i")
            self.assertEqual(instance.getInstanceId(), "uuid-1")
            self.assertEqual(instance.instance_id(retry=True), "uuid-1")
            self.assertEqual(instance.session_timeout_ms, 30000)
            configuration.set("client.key", "after")
            self.assertEqual(
                instance.getConfiguration().get("client.key"), "before"
            )
            clone = copy.copy(instance)
            self.addCleanup(clone.close)
            self.assertIsNot(clone.getConfiguration(), instance.getConfiguration())

            auth = AuthInfo("alice", b"s\x00cret", "uuid-1")
            self.assertEqual(auth.getUserName(), "alice")
            self.assertEqual(auth.username(), "alice")
            self.assertEqual(auth.getInstanceId(), "uuid-1")
            self.assertNotIn("s", repr(auth).split("password=", 1)[1])
            with self.assertRaisesRegex(AttributeError, "SB-DIV-002"):
                auth.getPassword()
            with self.assertRaisesRegex(AttributeError, "SB-DIV-002"):
                auth.password()
            with Connector(auth, instance, _api=api) as connector:
                self.assertIsInstance(connector.tableOps("t"), object)
                self.assertIsInstance(connector.securityOps(), object)
                self.assertIsInstance(connector.namespaceOps(), object)
                self.assertIsInstance(connector.tableInfo(), object)

        self.assertEqual(api.lib.resolve_timeouts, [0, 0])
        self.assertEqual(api.lib.identity_freed, 2)
        self.assertEqual(api.lib.connector_timeouts, [30000])

    def test_configuration_clone_and_numeric_defaults(self):
        configuration = Configuration()
        configuration.set("number", " 17tail")
        clone = copy.deepcopy(configuration)
        configuration.set("number", "99")
        self.assertEqual(clone.get("missing"), "")
        self.assertEqual(clone.get("missing", "fallback"), "fallback")
        self.assertEqual(clone.getLong("number"), 17)
        self.assertEqual(clone.getLong("missing", 23), 23)

    def test_zookeeper_instance_validation_and_safe_three_argument_default(self):
        api = FakeAPI()
        with self.assertRaisesRegex(ClientException, "instance name"):
            ZookeeperInstance(None, None, 1000, None, _api=api)
        instance = ZookeeperInstance("i", "zk", 1000, _api=api)
        self.addCleanup(instance.close)
        self.assertIsInstance(instance.getConfiguration(), Configuration)
        self.assertEqual(instance.session_timeout_ms, 1000)

    def test_auth_info_instance_mismatch_is_rejected_before_connect(self):
        api = FakeAPI()
        instance = ZookeeperInstance("i", "zk", 1000, Configuration(), _api=api)
        self.addCleanup(instance.close)
        with self.assertRaisesRegex(ValueError, "does not match"):
            Connector(AuthInfo("alice", "secret", "wrong"), instance, _api=api)
        self.assertEqual(api.lib.connector_timeouts, [])

    def test_bad_credentials_map_to_client_exception(self):
        api = FakeAPI()
        api.lib.connector_create_status = 8

        def check(status, error):
            del error
            if status:
                raise ClientException("bad credentials")

        api.check = check
        with self.assertRaisesRegex(ClientException, "bad credentials"):
            Connector("i", "zk", "alice", "wrong", _api=api)

    def test_connector_statistics_and_closed_lifecycle_are_stable(self):
        api = FakeAPI()
        connector = Connector("i", "zk", "u", "p", _api=api)
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-016"):
            connector.getStatistics()
        connector.close()
        with self.assertRaisesRegex(sharkbite.ClosedError, "closed"):
            connector.tableOps("t")

    def test_status_placeholders_preserve_dynamic_attributes(self):
        for status in (AccumuloInfo(), TabletServerStatus()):
            status.application_tag = "mine"
            self.assertEqual(status.application_tag, "mine")

    def test_connector_copy_is_explicitly_rejected(self):
        api = FakeAPI()
        with Connector("i", "zk", "u", "p", _api=api) as connector:
            with self.assertRaisesRegex(TypeError, "cannot be copied"):
                copy.copy(connector)
            with self.assertRaisesRegex(TypeError, "cannot be copied"):
                copy.deepcopy(connector)

    def test_client_scan_copies_and_frees_owned_result(self):
        api = FakeAPI()
        with Client("i", "zk", "u", "p", table="t", _api=api) as client:
            with client.scanner() as scanner:
                rows = scanner.scan(b"a", b"z")
        self.assertEqual(
            rows, [(Key(b"row", b"cf", b"cq", b"A", 7), b"value")]
        )
        self.assertEqual(api.lib.result_freed, 1)
        self.assertEqual(api.lib.closed, ["client"])
        self.assertEqual(api.lib.freed, ["client"])

    def test_scan_authorizations_are_copied_to_native_enforcement(self):
        api = FakeAPI()
        auths = [bytearray(b"blah1")]
        with Client("i", "zk", "u", "p", table="t", auths=auths, _api=api):
            auths[0][:] = b"other"
            self.assertEqual(api.lib.authorizations, [b"blah1"])

    def test_approved_divergence_is_explicit(self):
        client = object.__new__(Client)
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-016"):
            client.getStatistics()

    def test_accumulo_version_scope_is_explicit(self):
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-001"):
            Connector("i", "zk", "u", "p", accumulo_version="2.1.0", _api=FakeAPI())

    def test_scanner_options_and_python_iterator_are_importable_but_unsupported(self):
        self.assertEqual(int(ScannerOptions.HedgedReads), 1)
        self.assertEqual(int(ScannerOptions.RFileScanOnly), 2)
        iterator = PythonIterator("iter", 7).onNext("lambda key, value: True")
        self.assertEqual(iterator.getName(), "iter")
        self.assertEqual(iterator.priority(), 7)
        self.assertEqual(iterator.getClass(), "org.poma.accumulo.JythonIterator")
        with self.assertRaisesRegex(RuntimeError, "python script"):
            PythonIterator("iter", "class iter: pass", 7).onNext("lambda value: value")
        scanner = object.__new__(sharkbite.AccumuloScanner)
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-008"):
            scanner.setOption(ScannerOptions.HedgedReads)
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-008"):
            scanner.setOption(ScannerOptions.RFileScanOnly)
        with self.assertRaisesRegex(NotImplementedError, "SB-DIV-007"):
            scanner.addIterator(iterator)

    def test_unsupported_scanner_iterator_is_explicit(self):
        scanner = object.__new__(sharkbite.AccumuloScanner)
        with self.assertRaisesRegex(NotImplementedError, "use scan"):
            scanner.get(b"a")

    def test_status_and_compatibility_exception_mapping(self):
        self.assertIsInstance(exception_for_status(1, "bad"), InvalidArgumentError)
        self.assertIsInstance(exception_for_status(2, "handle"), InvalidHandleError)
        self.assertIsInstance(
            exception_for_status(15, "client", compatibility_class=1),
            ClientException,
        )
        table = exception_for_status(
            9,
            "table missing",
            compatibility_class=1,
            compatibility_code=9,
            source_class=1,
            source_name="ClientException",
        )
        self.assertIsInstance(table, ClientException)
        self.assertEqual(table.getErrorCode(), 9)
        self.assertEqual(CLIENT_ERROR_CODES[table.getErrorCode()], "Table not found in instance")
        illegal = exception_for_status(
            1,
            "bad argument",
            compatibility_class=0,
            source_class=6,
            source_name="IllegalArgumentException",
        )
        self.assertIs(type(illegal), RuntimeError)
        self.assertEqual(illegal.source_name, "IllegalArgumentException")
        hdfs = exception_for_status(
            9,
            "missing path",
            compatibility_class=0,
            source_class=5,
            source_name="HDFSException",
        )
        self.assertIs(type(hdfs), RuntimeError)
        self.assertEqual(hdfs.source_name, "HDFSException")

    def test_capability_negotiation_also_requires_dynamic_symbols(self):
        api = object.__new__(NativeAPI)
        api.capabilities = frozenset({CAP_HIGH_LEVEL_CLIENT})
        api._symbols = set(CAPABILITY_SYMBOLS[CAP_HIGH_LEVEL_CLIENT]) - {
            "shoal_client_create"
        }
        with self.assertRaisesRegex(NotImplementedError, "shoal_client_create"):
            api.require(CAP_HIGH_LEVEL_CLIENT)


if __name__ == "__main__":
    unittest.main()
