import java.io.DataInputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import org.apache.accumulo.core.crypto.CryptoUtils;
import org.apache.accumulo.core.data.ColumnUpdate;
import org.apache.accumulo.server.data.ServerMutation;
import org.apache.accumulo.tserver.logger.LogFileKey;
import org.apache.accumulo.tserver.logger.LogFileValue;

public final class VerifyNativeWAL172 {
  public static void main(String[] args) throws Exception {
    try (DataInputStream in = new DataInputStream(Files.newInputStream(Path.of(args[0])))) {
      byte[] magic = new byte["--- Log File Header (v4) ---".length()];
      in.readFully(magic);
      if (!Arrays.equals(magic, "--- Log File Header (v4) ---".getBytes(StandardCharsets.UTF_8)))
        throw new AssertionError("magic");
      byte[] params = CryptoUtils.readParams(in);
      if (!Arrays.equals(params, "U+1F47B".getBytes(StandardCharsets.UTF_8)))
        throw new AssertionError("crypto params");
      String[] rows = {"implicit", "max", "delete", "zero", "min"};
      boolean[] hasTimestamp = {false, true, false, true, true};
      long[] effectiveTimestamp = {123456789L, Long.MAX_VALUE, 987654321L, 0L,
          Long.MIN_VALUE};
      long[] systemTimestamp = {123456789L, 42L, 987654321L, 77L, 88L};
      boolean[] deleted = {false, false, true, false, true};
      byte[][] values = {{0, (byte)0xff, 1, (byte)0x80},
          "maximum".getBytes(StandardCharsets.UTF_8), {},
          "zero".getBytes(StandardCharsets.UTF_8), {}};
      for (int i = 0; i < 3; i++) {
        LogFileKey key = new LogFileKey();
        key.readFields(in);
        LogFileValue val = new LogFileValue();
        val.readFields(in);
        if (i == 0 && (!"OPEN".equals(key.getEvent().name()) || !val.getMutations().isEmpty()))
          throw new AssertionError("OPEN frame");
        if (i == 1 && (!"DEFINE_TABLET".equals(key.getEvent().name())
            || !val.getMutations().isEmpty()))
          throw new AssertionError("DEFINE_TABLET frame");
        if (i == 2 && (!"MANY_MUTATIONS".equals(key.getEvent().name())
            || val.getMutations().size() != rows.length))
          throw new AssertionError("MANY_MUTATIONS frame");
        System.out.printf("%s seq=%d tablet=%d mutations=%d%n", key.getEvent(), key.getSeq(),
            key.getTabletId(), val.getMutations().size());
        if (i == 0 && !"session\u0000\ud83d\udc7b".equals(key.getTserverSession()))
          throw new AssertionError("modified UTF-8 session");
        int j = 0;
        for (var m : val.getMutations()) {
          ServerMutation sm = (ServerMutation)m;
          ColumnUpdate u = sm.getUpdates().get(0);
          if (!rows[j].equals(new String(sm.getRow(), StandardCharsets.UTF_8))
              || sm.getSystemTimestamp() != systemTimestamp[j]
              || u.hasTimestamp() != hasTimestamp[j]
              || u.getTimestamp() != effectiveTimestamp[j]
              || u.isDeleted() != deleted[j]
              || !Arrays.equals(u.getValue(), values[j]))
            throw new AssertionError("mutation " + j + " changed on Java round trip");
          System.out.printf("%s system=%d explicit=%s ts=%d delete=%s value=%s%n",
              new String(sm.getRow(), StandardCharsets.UTF_8), sm.getSystemTimestamp(),
              u.hasTimestamp(), u.getTimestamp(), u.isDeleted(), Arrays.toString(u.getValue()));
          j++;
        }
      }
      if (in.read() != -1) throw new AssertionError("trailing bytes");
    }
  }
}
