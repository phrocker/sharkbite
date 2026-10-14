import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.Base64;
import java.util.Comparator;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;

import org.apache.accumulo.core.client.Accumulo;
import org.apache.accumulo.core.client.AccumuloClient;
import org.apache.accumulo.core.client.BatchWriter;
import org.apache.accumulo.core.client.Scanner;
import org.apache.accumulo.core.client.admin.CompactionConfig;
import org.apache.accumulo.core.client.admin.TableOperations;
import org.apache.accumulo.core.client.admin.servers.ServerId;
import org.apache.accumulo.core.conf.SiteConfiguration;
import org.apache.accumulo.core.data.Key;
import org.apache.accumulo.core.data.Mutation;
import org.apache.accumulo.core.data.Value;
import org.apache.accumulo.core.security.Authorizations;
import org.apache.accumulo.server.ServerContext;

public final class AccumuloSmoke {
  private static final String CLIENT_PROPERTIES =
      "/opt/accumulo/conf/accumulo-client.properties";
  private static final String TABLE = "shoal_accumulo4_smoke";
  private static final String RECOVERY_TABLE = "shoal_accumulo4_recovery";
  private static final String COMPACTION_TABLE = "shoal_accumulo4_compaction";
  private static final String SHOAL_TSERVER = "shoal-tserver:9997";

  private AccumuloSmoke() {}

  public static void main(String[] args) throws Exception {
    if (args.length < 1) {
      throw new IllegalArgumentException("expected conformance mode");
    }
    if ("system-token".equals(args[0])) {
      try (ServerContext context = new ServerContext(SiteConfiguration.auto())) {
        byte[] token = context.getCredentials().toThrift(context.getInstanceID()).getToken();
        System.out.println(Base64.getEncoder().encodeToString(token));
      }
      return;
    }
    try (AccumuloClient client = Accumulo.newClient().from(CLIENT_PROPERTIES).build()) {
      if ("ready-manager".equals(args[0])) {
        readyManager(client);
      } else if ("ready".equals(args[0])) {
        ready(client);
      } else if ("smoke".equals(args[0])) {
        smoke(client);
      } else if ("shoal-ready".equals(args[0])) {
        requireServer(client, SHOAL_TSERVER, true);
      } else if ("shoal-absent".equals(args[0])) {
        requireServer(client, SHOAL_TSERVER, false);
      } else if ("tserver".equals(args[0])) {
        tserver(client);
      } else if ("recovery-prepare".equals(args[0])) {
        recoveryPrepare(client);
      } else if ("recovery-verify".equals(args[0])) {
        verifyRows(client, RECOVERY_TABLE, 64);
      } else if ("compactor".equals(args[0])) {
        compactor(client);
      } else {
        throw new IllegalArgumentException("unknown conformance mode " + args[0]);
      }
    }
  }

  private static void requireServer(AccumuloClient client, String address, boolean present) {
      boolean found = client.instanceOperations().getServers(ServerId.Type.TABLET_SERVER).stream()
          .anyMatch(server -> address.equals(server.getHost() + ":" + server.getPort()));
      if (found != present) {
        throw new IllegalStateException("server " + address + " present=" + found
            + ", expected " + present);
      }
      System.out.println("SHOAL_EVIDENCE service-lock=" + (present ? "present" : "fenced"));
    }

    private static void tserver(AccumuloClient client) throws Exception {
      recreate(client.tableOperations(), TABLE);
      writeRows(client, TABLE, 32);
      client.tableOperations().flush(TABLE, null, null, true);
      String digest = verifyRows(client, TABLE, 32);
      System.out.println("SHOAL_EVIDENCE assignment=shoal java-write-flush-scan=" + digest
          + " continuation=batch-size-1 minor-compaction=published");
    }

    private static void recoveryPrepare(AccumuloClient client) throws Exception {
      recreate(client.tableOperations(), RECOVERY_TABLE);
      writeRows(client, RECOVERY_TABLE, 64);
      System.out.println("SHOAL_EVIDENCE wal=prepared rows=64");
    }

    private static void compactor(AccumuloClient client) throws Exception {
      TableOperations tables = client.tableOperations();
      System.out.println("SHOAL_PROGRESS compactor=recreate-table");
      recreate(tables, COMPACTION_TABLE);
      for (int batch = 0; batch < 4; batch++) {
        System.out.println("SHOAL_PROGRESS compactor=write-flush batch=" + batch);
        try (BatchWriter writer = client.createBatchWriter(COMPACTION_TABLE)) {
          for (int row = batch * 16; row < (batch + 1) * 16; row++) {
            put(writer, row);
          }
        }
        tables.flush(COMPACTION_TABLE, null, null, true);
      }
      System.out.println("SHOAL_PROGRESS compactor=verify-before");
      String before = verifyRows(client, COMPACTION_TABLE, 64);
      System.out.println("SHOAL_PROGRESS compactor=request-waiting");
      tables.setProperty(COMPACTION_TABLE, "table.compaction.dispatcher.opts.service", "shoal");
      tables.compact(COMPACTION_TABLE, new CompactionConfig().setWait(true));
      System.out.println("SHOAL_PROGRESS compactor=verify-after");
      String after = verifyRows(client, COMPACTION_TABLE, 64);
      if (!before.equals(after)) {
        throw new IllegalStateException("promotion changed canonical cells: "
            + before + " != " + after);
      }
      for (int batch = 0; batch < 3; batch++) {
        System.out.println("SHOAL_PROGRESS compactor=write-history batch=" + batch);
        try (BatchWriter writer = client.createBatchWriter(COMPACTION_TABLE)) {
          for (int row = 0; row < 64; row++) {
            Mutation mutation = new Mutation(String.format("row-%04d", row));
            mutation.put("cf", "cq", 0L,
                new Value(("historical-" + batch).getBytes(StandardCharsets.UTF_8)));
            writer.addMutation(mutation);
          }
        }
        tables.flush(COMPACTION_TABLE, null, null, true);
      }
      System.out.println("SHOAL_PROGRESS compactor=request-cancel");
      tables.compact(COMPACTION_TABLE, new CompactionConfig().setWait(false));
      tables.cancelCompaction(COMPACTION_TABLE);
      System.out.println("SHOAL_PROGRESS compactor=verify-cancel");
      String afterCancel = verifyRows(client, COMPACTION_TABLE, 64);
      if (!after.equals(afterCancel)) {
        throw new IllegalStateException("cancellation changed canonical cells: "
            + after + " != " + afterCancel);
      }
      System.out.println("SHOAL_EVIDENCE external-compaction=completed publication=visible"
          + " java-readable=" + after + " promotion-equivalent=true"
          + " cancellation=completed-with-readable-table");
    }

    private static void recreate(TableOperations tables, String table) throws Exception {
      if (tables.exists(table)) {
        tables.delete(table);
      }
      tables.create(table);
    }

    private static void writeRows(AccumuloClient client, String table, int rows) throws Exception {
      try (BatchWriter writer = client.createBatchWriter(table)) {
        for (int row = 0; row < rows; row++) {
          put(writer, row);
        }
        writer.flush();
      }
    }

    private static void put(BatchWriter writer, int row) throws Exception {
      Mutation mutation = new Mutation(String.format("row-%04d", row));
      mutation.put("cf", "cq", new Value(("value-" + row).getBytes(StandardCharsets.UTF_8)));
      writer.addMutation(mutation);
    }

    private static String verifyRows(AccumuloClient client, String table, int expected)
        throws Exception {
      List<String> cells = new ArrayList<>();
      try (Scanner scanner = client.createScanner(table, Authorizations.EMPTY)) {
        scanner.setBatchSize(1);
        for (Map.Entry<Key,Value> entry : scanner) {
          cells.add(entry.getKey().toStringNoTime() + "=" + entry.getValue());
        }
      }
      cells.sort(Comparator.naturalOrder());
      if (cells.size() != expected) {
        throw new IllegalStateException("expected " + expected + " cells, got " + cells.size());
      }
      MessageDigest digest = MessageDigest.getInstance("SHA-256");
      for (String cell : cells) {
        digest.update(cell.getBytes(StandardCharsets.UTF_8));
        digest.update((byte) '\n');
      }
      return HexFormat.of().formatHex(digest.digest());
    }

  private static void ready(AccumuloClient client) throws Exception {
    readyManager(client);
    if (client.instanceOperations().getServers(ServerId.Type.TABLET_SERVER).isEmpty()) {
      throw new IllegalStateException("no tablet server is registered");
    }
  }

  private static void readyManager(AccumuloClient client) throws Exception {
    if (client.instanceOperations().getServers(ServerId.Type.MANAGER).isEmpty()) {
      throw new IllegalStateException("manager ServiceLock is unavailable");
    }
  }

  private static void smoke(AccumuloClient client) throws Exception {
    TableOperations tables = client.tableOperations();
    if (tables.exists(TABLE)) {
      tables.delete(TABLE);
    }
    tables.create(TABLE);
    try {
      Mutation mutation = new Mutation("row-1");
      mutation.put("cf", "cq", new Value("value-1".getBytes(StandardCharsets.UTF_8)));
      try (BatchWriter writer = client.createBatchWriter(TABLE)) {
        writer.addMutation(mutation);
        writer.flush();
      }
      tables.flush(TABLE, null, null, true);

      int cells = 0;
      try (Scanner scanner = client.createScanner(TABLE, Authorizations.EMPTY)) {
        for (Map.Entry<Key,Value> entry : scanner) {
          cells++;
          Key key = entry.getKey();
          if (!"row-1".equals(key.getRow().toString())
              || !"cf".equals(key.getColumnFamily().toString())
              || !"cq".equals(key.getColumnQualifier().toString())
              || !"value-1".equals(entry.getValue().toString())) {
            throw new IllegalStateException("unexpected cell: " + entry);
          }
        }
      }
      if (cells != 1) {
        throw new IllegalStateException("expected exactly one cell, got " + cells);
      }
    } finally {
      if (tables.exists(TABLE)) {
        tables.delete(TABLE);
      }
    }
  }
}
