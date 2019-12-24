package com.mjwall.accumulo;

import java.io.File;
import java.io.IOException;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.file.FileVisitResult;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.SimpleFileVisitor;
import java.nio.file.attribute.BasicFileAttributes;
import java.util.Map;

import com.mjwall.accumulo.commands.SmacAccumuloShell;
import com.mjwall.accumulo.config.RunningEnv;
import org.apache.zookeeper.KeeperException;
import org.apache.accumulo.core.client.Instance;
import org.apache.accumulo.core.client.ZooKeeperInstance;
import org.apache.accumulo.core.conf.Property;
import org.apache.accumulo.core.util.MonitorUtil;
import org.apache.accumulo.minicluster.impl.MiniAccumuloClusterImpl;
import org.apache.accumulo.minicluster.impl.MiniAccumuloConfigImpl;
import org.apache.accumulo.monitor.Monitor;

/**
 * Run a standalone mini accumulo cluster until killed
 * <p>
 * A Mini Accumulo cluster will start the following services: Zookeeper Master Monitor GC 2 - TServers Then an interactive shell will start and the cluster will
 * run until the shell is closed
 */
public class StandaloneMAC {

  public static void main(String[] args) {

    File tempDir = null;
    MiniAccumuloClusterImpl cluster = null;
    boolean purgeTemp = false;

    try {
      String tempMiniDir = System.getProperty("tempMiniDir", null);
      // not sure this works yet
      boolean reuseTempMiniDir = Boolean.parseBoolean(System.getProperty("reuseTempMiniDir", "false"));

      if (tempMiniDir == null) {
        tempDir = com.google.common.io.Files.createTempDir();
        tempDir.deleteOnExit();
        purgeTemp = true;
      } else {
        tempDir = new File(tempMiniDir);
        if (tempDir.exists()) {
          if (!reuseTempMiniDir) {
            throw new RuntimeException("tempMiniDir directory was not empty: " + tempMiniDir);
            // be safer about deleting
            // recursiveDelete(tempDir.toPath());
          } else {
            System.out.println("tempMiniDir existed at " + tempMiniDir + " but reuseTempMiniDir was true");
          }
        }
        tempDir.mkdir();
      }

      // this doesn't work
      // final String rootUsername = System.getProperty("rootUseranme", "root");
      final String rootPassword = System.getProperty("rootPassword", "secret");
      final String instanceName = System.getProperty("instanceName", "smac");
      final String existingZookeepers = System.getProperty("existingZookeepers");
      final int zookeeperPort = Integer.parseInt(System.getProperty("zookeeperPort", "-1"));
      if ((zookeeperPort != -1) && (existingZookeepers != null)) {
        System.out.println("Tried setting both existing Zookeeper and Zookeeper port, ignoring the port");
      }

      MiniAccumuloConfigImpl config = new MiniAccumuloConfigImpl(tempDir, rootPassword);
      config.setInstanceName(instanceName);
      final int numTservers = Integer.parseInt(System.getProperty("numTservers", "2"));
      config.setNumTservers(numTservers);
      if (existingZookeepers != null) {
        config.setExistingZooKeepers(existingZookeepers);
      } else {
        try (Socket ignored = new Socket("localhost", zookeeperPort)) {
          throw new RuntimeException("Zookeeper can't bind to port already in use: " + zookeeperPort);
        } catch (IOException available) {
          config.setZooKeeperPort(zookeeperPort);
        }
      }

      Map<String,String> siteConfig = config.getSiteConfig();
      final int monitorPort = Integer.parseInt(System.getProperty("monitorPort"));
      try (Socket ignored = new Socket("localhost", monitorPort)) {
        throw new RuntimeException("Monitor can't bind to port already in use: " + monitorPort);
      } catch (IOException available) {
        siteConfig.put(Property.MONITOR_PORT.getKey(), Integer.toString(monitorPort));
      }
      final String tserverClientPort = System.getProperty("tserverClientPort", "9997");
      siteConfig.put(Property.TSERV_CLIENTPORT.getKey(), tserverClientPort);
      config.setSiteConfig(siteConfig);

      final boolean setJDWP = Boolean.valueOf(System.getProperty("setJDWP", "false"));
      if (setJDWP) {
        config.setJDWPEnabled(true);
      }

      cluster = new MiniAccumuloClusterImpl(config);

      String log4jConfig = System.getProperty("log4jConfig", null);
      // no check on file exists for log4j.configuration
      String origLog4jConfiguration = System.getProperty("log4j.configuration", null);

      if (null != log4jConfig) {
        if (null != origLog4jConfiguration) {
          System.out.println("Original log4j.configuration: " + origLog4jConfiguration + " - trying to overwrite");
        }
        if (new File(log4jConfig).exists()) {
          // see https://github.com/apache/accumulo/blob/rel/1.9.3/server/base/src/main/java/org/apache/accumulo/server/Accumulo.java#L126
          System.out.println("Setting log4j.configuration to: " + log4jConfig);
          System.setProperty("log4j.configuration", log4jConfig);
        } else {
          System.err.println("log4jConfig doesn't exist at " + log4jConfig + " setting to null");
          log4jConfig = null;
        }
      } else if (null != origLog4jConfiguration) {
        System.out.println("You set log4j.configuration but not log4jConfig, making them the same");
        log4jConfig = origLog4jConfiguration;
      }

      // now append to config system properties if not null
      if (null != log4jConfig) {
        Map<String,String> sysProps = config.getSystemProperties();
        sysProps.put("log4j.configuration", log4jConfig);
        config.setSystemProperties(sysProps);
      }

      cluster.start(); // starts zookeeper, tablet servers, gc and master

      cluster.exec(Monitor.class);

      // Get monitor location to ensure it is running.
      String monitorLocation = null;
      for (int i = 0; i < 5; i++) {
        Thread.sleep(5 * 1000);
        try {
          Instance instance = new ZooKeeperInstance(cluster.getClientConfig());
          monitorLocation = MonitorUtil.getLocation(instance);
          if (monitorLocation != null) {
            break;
          }
        } catch (KeeperException e) {
          System.out.println("Waiting for zookeeper");
          // e.printStackTrace();
        }
      }
      String monitorUrl = "";
      if (monitorLocation == null) {
        System.err.println("Monitor:        not started");
      } else {
        monitorUrl = "http://localhost:" + monitorLocation.split(":")[1];
      }
      String initScript = System.getProperty("initScript", null);
      boolean startShell = Boolean.valueOf(System.getProperty("startShell", "true"));

      if (initScript != null) {
        Path script = Paths.get(initScript);
        if (Files.exists(script)) {
          ProcessBuilder pb = new ProcessBuilder(script.toAbsolutePath().toString());
          pb.inheritIO();
          System.out.println("Running init script: " + initScript);
          System.out.println("--------------------");
          Process p = pb.start();
          int exitCode = p.waitFor();
          System.out.println("--------------------");
          System.out.println("init script ended with: " + exitCode);
        } else {
          System.err.println("Tried to run the following init script but it didn't exist: " + initScript);
        }
      }

      int shutdownPort = Integer.parseInt(System.getProperty("shutdownPort", "4445"));

      RunningEnv env = new RunningEnv(cluster, reuseTempMiniDir, monitorUrl, initScript, startShell, setJDWP, shutdownPort, log4jConfig, tserverClientPort);
      env.toStdOut();
      env.writeEnvFile();

      final MiniAccumuloClusterImpl finalCluster = cluster;
      final boolean finalPurgeTemp = purgeTemp;
      final File finalTempDir = tempDir;
      Runtime.getRuntime().addShutdownHook(new Thread() {
        @Override
        public void run() {
          System.out.println("Stopping Mini Accumulo");
          try {
            finalCluster.stop();
            if (finalPurgeTemp) {
              Thread.sleep(3000);
              recursiveDelete(finalTempDir.toPath());
            }
          } catch (IOException | InterruptedException e) {
            // TODO Auto-generated catch block
            e.printStackTrace();
          }
          try {
            System.out.println("Removing " + RunningEnv.RUNNING_ENV_FILE.toAbsolutePath().toString());
            Files.delete(RunningEnv.RUNNING_ENV_FILE);
          } catch (IOException e) {
            e.printStackTrace();
          }
        }
      });

      if (startShell) {
        System.out.println("Type exit to shutdown the SMAC, shutdown port ignored");
        System.out.println("Or try the shutdown port with something like `curl telnet://localhost:" + shutdownPort + "`");
        new SmacAccumuloShell().startWithShutdown();
      } else {
        System.out.println("Listening for shutdown on port " + shutdownPort);
        System.out.println("You can hit Ctrl-C if you are running this with java -jar");
        System.out.println("Use `docker stop <containerid>` if running this in docker");
        System.out.println("Or try the shutdown port with something like `curl telnet://localhost:" + shutdownPort + "`");
        // start a socket on the shutdown port and block- anything connection will continue
        try (ServerSocket shutdownServer = new ServerSocket(shutdownPort)) {
          shutdownServer.accept().close();
        }
      }
    } catch (IOException | InterruptedException error) {
      System.err.println(error.getMessage());
      error.printStackTrace();
    } catch (Exception e) {
      System.err.println(e.getMessage());
      e.printStackTrace();
    }

  }

  private static void recursiveDelete(Path dir) {
    System.out.println("Deleting dir recursively: " + dir.toString());
    try {
      java.nio.file.Files.walkFileTree(dir, new SimpleFileVisitor<Path>() {
        @Override
        public FileVisitResult visitFile(Path file, BasicFileAttributes attrs) throws IOException {
          java.nio.file.Files.delete(file);
          return FileVisitResult.CONTINUE;
        }

        @Override
        public FileVisitResult postVisitDirectory(Path dir, IOException exc) throws IOException {
          java.nio.file.Files.delete(dir);
          return FileVisitResult.CONTINUE;
        }
      });
    } catch (IOException e) {
      // TODO Auto-generated catch block
      e.printStackTrace();
    }
  }
}
