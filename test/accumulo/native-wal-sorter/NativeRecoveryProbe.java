import java.lang.reflect.Field;
import java.lang.reflect.InvocationTargetException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Set;
import org.apache.accumulo.core.conf.AccumuloConfiguration;
import org.apache.accumulo.core.conf.DefaultConfiguration;
import org.apache.accumulo.core.conf.SiteConfiguration;
import org.apache.accumulo.core.data.Mutation;
import org.apache.accumulo.core.data.TableId;
import org.apache.accumulo.core.dataImpl.KeyExtent;
import org.apache.accumulo.core.spi.crypto.CryptoServiceFactory;
import org.apache.accumulo.core.spi.crypto.GenericCryptoServiceFactory;
import org.apache.accumulo.core.spi.crypto.NoCryptoService;
import org.apache.accumulo.core.tabletserver.log.LogEntry;
import org.apache.accumulo.server.ServerContext;
import org.apache.accumulo.server.data.ServerMutation;
import org.apache.accumulo.server.fs.VolumeManager;
import org.apache.accumulo.server.fs.VolumeManagerImpl;
import org.apache.accumulo.server.log.SortedLogState;
import org.apache.accumulo.tserver.log.LogSorter;
import org.apache.accumulo.tserver.log.ResolvedSortedLog;
import org.apache.accumulo.tserver.log.SortedLogRecovery;
import org.apache.hadoop.fs.Path;
import org.apache.hadoop.io.Text;
import sun.misc.Unsafe;

// Calls the pinned, unmodified LogSorter and SortedLogRecovery classes on a
// WAL produced by Sharkbite's Go NativeWriter. No Accumulo cluster is needed.
public final class NativeRecoveryProbe {
  private static final Unsafe UNSAFE;
  static {
    try {
      Field field = Unsafe.class.getDeclaredField("theUnsafe");
      field.setAccessible(true);
      UNSAFE = (Unsafe) field.get(null);
    } catch (ReflectiveOperationException e) {
      throw new ExceptionInInitializerError(e);
    }
  }

  private static final class LocalContext extends ServerContext {
    private VolumeManager volumeManager;

    // Unsafe.allocateInstance bypasses this constructor. The real constructor
    // needs ZooKeeper; only these three methods are used by the recovery path.
    private LocalContext() {
      super(SiteConfiguration.empty().build());
      throw new AssertionError("LocalContext constructor must not run");
    }

    @Override public VolumeManager getVolumeManager() { return volumeManager; }
    @Override public AccumuloConfiguration getConfiguration() {
      return DefaultConfiguration.getInstance();
    }
    @Override public CryptoServiceFactory getCryptoFactory() {
      return new GenericCryptoServiceFactory();
    }
  }

  private static void setField(Object object, String name, Object value) throws Exception {
    Field field = object.getClass().getDeclaredField(name);
    field.setAccessible(true);
    field.set(object, value);
  }

  private static void require(boolean condition, String message) {
    if (!condition) throw new AssertionError(message);
  }

  private static void assertRows(List<Mutation> got, String... expected) {
    require(got.size() == expected.length, "recovered " + got.size() + " rows, want " + expected.length);
    for (int i = 0; i < expected.length; i++) {
      String row = new String(got.get(i).getRow(), StandardCharsets.UTF_8);
      require(row.equals(expected[i]), "row " + i + " = " + row + ", want " + expected[i]);
      require(got.get(i) instanceof ServerMutation, "lost server mutation at " + row);
      require(got.get(i).getUpdates().size() == 1, "update count for " + row);
      var update = got.get(i).getUpdates().get(0);
      byte[] want = switch (row) {
        case "left" -> "left-value".getBytes(StandardCharsets.UTF_8);
        case "right" -> "right-value".getBytes(StandardCharsets.UTF_8);
        case "batch-a" -> new byte[] {0, (byte) 0xff, 1};
        case "batch-b" -> new byte[0];
        default -> throw new AssertionError("unexpected row " + row);
      };
      require(Arrays.equals(update.getValue(), want), "value bytes changed for " + row);
      require(Arrays.equals(update.getColumnFamily(), "cf".getBytes(StandardCharsets.UTF_8)),
          "column family changed for " + row);
      require(Arrays.equals(update.getColumnQualifier(), "cq".getBytes(StandardCharsets.UTF_8)),
          "column qualifier changed for " + row);
      require(Arrays.equals(update.getColumnVisibility(), "vis".getBytes(StandardCharsets.UTF_8)),
          "column visibility changed for " + row);
      long systemTime = switch (row) {
        case "left" -> 101;
        case "right" -> 202;
        case "batch-a" -> 102;
        case "batch-b" -> 103;
        default -> throw new AssertionError("unexpected row " + row);
      };
      require(((ServerMutation) got.get(i)).getSystemTimestamp() == systemTime,
          "system timestamp changed for " + row);
      require(update.isDeleted() == row.equals("batch-b"), "delete flag changed for " + row);
      require(update.hasTimestamp() == row.equals("batch-b"), "timestamp flag changed for " + row);
      if (row.equals("batch-b")) require(update.getTimestamp() == 0, "explicit zero timestamp changed");
    }
  }

  public static void main(String[] args) throws Exception {
    require(args.length == 2, "expected WAL and workdir paths");
    String wal = "file://" + args[0];
    String destination = "file://" + args[1] + "/recovery/00000000-0000-0000-0000-000000000172";
    try (VolumeManager vm = VolumeManagerImpl.getLocalForTesting(args[1])) {
      LocalContext context = (LocalContext) UNSAFE.allocateInstance(LocalContext.class);
      context.volumeManager = vm;
      LogSorter sorter = (LogSorter) UNSAFE.allocateInstance(LogSorter.class);
      setField(sorter, "context", context);
      setField(sorter, "cryptoService", new NoCryptoService());
      setField(sorter, "sortedLogConf", DefaultConfiguration.getInstance());

      // process() catches exceptions and writes a failed marker. Calling sort()
      // directly makes every parser or RFile error fail this executable gate.
      Class<?> processorClass = Class.forName("org.apache.accumulo.tserver.log.LogSorter$LogProcessor");
      var constructor = processorClass.getDeclaredConstructor(LogSorter.class);
      constructor.setAccessible(true);
      Object processor = constructor.newInstance(sorter);
      var sort = processorClass.getDeclaredMethod("sort", VolumeManager.class, String.class,
          Path.class, String.class);
      sort.setAccessible(true);
      try {
        sort.invoke(processor, vm, "native-proof", new Path(wal), destination);
      } catch (InvocationTargetException e) {
        throw new AssertionError("native LogSorter rejected Go WAL", e.getCause());
      }
      require(vm.exists(SortedLogState.getFinishedMarkerPath(destination)), "no finished marker");
      require(!vm.exists(SortedLogState.getFailedMarkerPath(destination)), "sort failed marker");
      ResolvedSortedLog resolved = ResolvedSortedLog.resolve(LogEntry.fromPath(wal), vm);
      require(!resolved.getChildren().isEmpty(), "empty sorted RFile set");

      SortedLogRecovery recovery = new SortedLogRecovery(context, null, null);
      KeyExtent left = new KeyExtent(TableId.of("g"), new Text("m"), null);
      KeyExtent right = new KeyExtent(TableId.of("g"), new Text("z"), new Text("m"));
      List<Mutation> leftRows = new ArrayList<>();
      List<Mutation> rightRows = new ArrayList<>();
      recovery.recover(left, List.of(resolved), Set.of(), leftRows::add);
      recovery.recover(right, List.of(resolved), Set.of(), rightRows::add);
      assertRows(leftRows, "left", "batch-a", "batch-b");
      assertRows(rightRows, "right");
      System.out.println("PASS: pinned LogSorter + SortedLogRecovery replayed Go WAL across two tablets");
    }
  }
}
