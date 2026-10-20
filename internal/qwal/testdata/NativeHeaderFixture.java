// Header-only fixture oracle for Accumulo revision
// 1a716b2c1bb5762ead4b46d2bc4f53e13873b314 (the pinned test image).
// Compile against that revision's NoCryptoService, NoFileEncrypter, and
// CryptoUtils classes, then run with the same classes first on the classpath.
// It deliberately does not assert mutation/recovery parity.
import java.io.DataOutputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.charset.StandardCharsets;
import org.apache.accumulo.core.crypto.CryptoUtils;
import org.apache.accumulo.core.spi.crypto.NoFileEncrypter;

public final class NativeHeaderFixture {
  public static void main(String[] args) throws Exception {
    if (args.length != 1) {
      throw new IllegalArgumentException("expected output path");
    }
    try (DataOutputStream out = new DataOutputStream(Files.newOutputStream(Path.of(args[0])))) {
      // DfsLogger.LOG_FILE_HEADER_V4 at the pinned revision.
      out.write("--- Log File Header (v4) ---".getBytes(StandardCharsets.UTF_8));
      CryptoUtils.writeParams(new NoFileEncrypter().getDecryptionParameters(), out);
      out.writeByte(0xFA); // first-entry marker, to prove header cursor alignment
    }
  }
}
