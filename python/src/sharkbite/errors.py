from __future__ import annotations


class ShoalError(RuntimeError):
    """Base exception raised by the Shoal binding."""

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        source_class: int | None = None,
        source_name: str | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.source_class = source_class
        self.source_name = source_name


class ClientException(ShoalError):
    """Compatibility exception for Sharkbite client failures."""

    def __init__(
        self,
        message: str,
        *,
        status: int | None = None,
        error_code: int = -1,
        source_class: int | None = None,
        source_name: str | None = None,
    ) -> None:
        super().__init__(
            message,
            status=status,
            source_class=source_class,
            source_name=source_name,
        )
        self.error_code = error_code

    def getErrorCode(self) -> int:
        return self.error_code


class InvalidArgumentError(ShoalError):
    pass


class InvalidHandleError(ShoalError):
    pass


class ClosedError(ShoalError):
    pass


class CancelledError(ShoalError):
    pass


class DeadlineExceededError(ShoalError):
    pass


class NotFoundError(ShoalError):
    pass


class PermissionDeniedError(ShoalError):
    pass


class AlreadyExistsError(ShoalError):
    pass


class UnsupportedError(NotImplementedError, ShoalError):
    pass


class OutOfMemoryError(ShoalError):
    pass


class BootstrapError(ShoalError):
    pass


class UnavailableError(ShoalError):
    pass


class DiscoveryUnavailableError(UnavailableError):
    pass


class TabletUnavailableError(UnavailableError):
    pass


class SecurityUnavailableError(UnavailableError):
    pass


class RangeSpansTabletsError(ShoalError):
    pass


class CleanupError(ShoalError):
    pass


class OperationError(ShoalError):
    pass


class RetryExhaustedError(ShoalError):
    pass


class MutationRejectedError(ShoalError):
    pass


class AmbiguousWriteError(ShoalError):
    pass


class NamespaceNotEmptyError(ShoalError):
    pass


class TableOfflineError(ShoalError):
    pass


class UserNotFoundError(ShoalError):
    pass


class BadCredentialsError(PermissionDeniedError):
    pass


class IncompleteError(ShoalError):
    pass


class InternalError(ShoalError):
    pass


STATUS_EXCEPTIONS: dict[int, type[BaseException]] = {
    1: InvalidArgumentError,
    2: InvalidHandleError,
    3: OutOfMemoryError,
    4: UnsupportedError,
    5: BootstrapError,
    6: ClosedError,
    7: CancelledError,
    8: DeadlineExceededError,
    9: NotFoundError,
    10: PermissionDeniedError,
    11: DiscoveryUnavailableError,
    12: TabletUnavailableError,
    13: RangeSpansTabletsError,
    14: CleanupError,
    15: OperationError,
    16: RetryExhaustedError,
    17: MutationRejectedError,
    18: AmbiguousWriteError,
    19: AlreadyExistsError,
    20: UnavailableError,
    21: NamespaceNotEmptyError,
    22: TableOfflineError,
    23: UserNotFoundError,
    24: BadCredentialsError,
    25: SecurityUnavailableError,
    26: IncompleteError,
    255: InternalError,
}

CLIENT_ERROR_CODES = {
    0: "Invalid return from zookeeper",
    1: "Invalid ZK server string retrieved from Zookeeper",
    2: "Invalid ZK server port",
    3: "Invalid server port",
    4: "No master running at specified host and port",
    5: "Invalid username and password combination",
    6: "Could not create namespace",
    7: "Cannot Delete default namespace",
    8: "Could not locate tablet",
    9: "Table not found in instance",
    10: "Range not supplied for scanner",
    11: "The table or namespace must not be empty",
    12: "Function argument cannot be null or empty",
    13: "Options cannot be set on a scanner after iteration of results has begun",
}


class NativeErrorContext(RuntimeError):
    """Stable native status/source context chained to compatibility exceptions."""

    def __init__(self, status: int, source_class: int, source_name: str) -> None:
        super().__init__(f"Shoal status {status} from {source_name}")
        self.status = status
        self.source_class = source_class
        self.source_name = source_name


def exception_for_status(
    status: int,
    message: str,
    *,
    compatibility_class: int | None = None,
    compatibility_code: int = -1,
    source_class: int | None = None,
    source_name: str | None = None,
) -> BaseException:
    if compatibility_class == 1:
        return ClientException(
            message,
            status=status,
            error_code=compatibility_code,
            source_class=source_class,
            source_name=source_name,
        )
    if compatibility_class == 0:
        exc = RuntimeError(message)
        setattr(exc, "status", status)
        setattr(exc, "source_class", source_class)
        setattr(exc, "source_name", source_name)
        return exc
    cls = STATUS_EXCEPTIONS.get(status)
    if cls is None:
        cls = ShoalError
    return cls(  # type: ignore[call-arg]
        message,
        status=status,
        source_class=source_class,
        source_name=source_name,
    )
