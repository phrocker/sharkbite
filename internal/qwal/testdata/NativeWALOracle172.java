// Java WAL serialization oracle for pinned Accumulo
// 1a716b2c1bb5762ead4b46d2bc4f53e13873b314.
// Compile with pinned-source Mutation, ServerMutation, LogFileKey, LogFileValue,
// CryptoUtils, NoCryptoService, NoFileEncrypter and compatible dependency jars.
import java.io.DataOutputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import org.apache.accumulo.core.crypto.CryptoUtils;
import org.apache.accumulo.core.data.TableId;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.dataImpl.KeyExtent;
import org.apache.accumulo.core.spi.crypto.NoFileEncrypter;
import org.apache.accumulo.server.data.ServerMutation;
import org.apache.accumulo.tserver.logger.LogEvents;
import org.apache.accumulo.tserver.logger.LogFileKey;
import org.apache.accumulo.tserver.logger.LogFileValue;
import org.apache.hadoop.io.Text;

public final class NativeWALOracle172 {
  private static void write(DataOutputStream out, LogFileKey key, List<ServerMutation> mutations)
      throws Exception {
    key.write(out);
    LogFileValue value = new LogFileValue();
    value.setMutations(List.copyOf(mutations));
    value.write(out);
  }

  private static LogFileKey key(LogEvents event, long seq, int tabletId) {
    LogFileKey key = new LogFileKey();
    key.setEvent(event);
    key.setSeq(seq);
    key.setTabletId(tabletId);
    return key;
  }

  public static void main(String[] args) throws Exception {
    if (args.length != 1) throw new IllegalArgumentException("output path");
    Text cf = new Text("cf");
    Text cq = new Text("cq");
    ServerMutation implicit = new ServerMutation(new Text("implicit"));
    implicit.put(cf, cq, new Value(new byte[]{0, (byte)0xff, 1, (byte)0x80}));
    implicit.setSystemTimestamp(123456789L);
    ServerMutation max = new ServerMutation(new Text("max"));
    max.put(cf, cq, Long.MAX_VALUE, new Value("maximum".getBytes(StandardCharsets.UTF_8)));
    max.setSystemTimestamp(42L);
    ServerMutation deleted = new ServerMutation(new Text("delete"));
    deleted.putDelete(cf, cq);
    deleted.setSystemTimestamp(987654321L);
    ServerMutation zero = new ServerMutation(new Text("zero"));
    zero.put(cf, cq, 0L, new Value("zero".getBytes(StandardCharsets.UTF_8)));
    zero.setSystemTimestamp(77L);
    ServerMutation min = new ServerMutation(new Text("min"));
    min.putDelete(cf, cq, Long.MIN_VALUE);
    min.setSystemTimestamp(88L);

    try (DataOutputStream out = new DataOutputStream(Files.newOutputStream(Path.of(args[0])))) {
      out.write("--- Log File Header (v4) ---".getBytes(StandardCharsets.UTF_8));
      CryptoUtils.writeParams(new NoFileEncrypter().getDecryptionParameters(), out);
      LogFileKey open = key(LogEvents.OPEN, -1, -1);
      open.setTserverSession("session\u0000\ud83d\udc7b"); // Java modified UTF-8 edge cases
      write(out, open, List.of());
      LogFileKey define = key(LogEvents.DEFINE_TABLET, 10, 7);
      define.setTablet(new KeyExtent(TableId.of("g"), new Text("z"), null));
      write(out, define, List.of());
      write(out, key(LogEvents.MANY_MUTATIONS, 11, 7),
          List.of(implicit, max, deleted, zero, min));
    }
  }
}
