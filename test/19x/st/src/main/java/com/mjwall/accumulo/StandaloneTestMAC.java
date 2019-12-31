package com.mjwall.accumulo;

import com.mjwall.accumulo.commands.SmacAccumuloShell;
import com.mjwall.accumulo.config.RunningEnv;
import org.apache.accumulo.core.client.Instance;
import org.apache.accumulo.core.client.ZooKeeperInstance;
import org.apache.accumulo.core.conf.Property;
import org.apache.accumulo.core.util.MonitorUtil;
import org.apache.accumulo.minicluster.impl.MiniAccumuloClusterImpl;
import org.apache.accumulo.minicluster.impl.MiniAccumuloConfigImpl;
import org.apache.accumulo.monitor.Monitor;
import org.apache.commons.io.FileUtils;
import org.apache.zookeeper.KeeperException;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.OutputStream;
import java.io.PrintStream;
import java.io.PrintWriter;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.file.FileVisitResult;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.SimpleFileVisitor;
import java.nio.file.attribute.BasicFileAttributes;
import java.util.Map;
import java.util.Random;
import java.util.UUID;

/**
 * Run a standalone mini accumulo cluster until killed
 * <p>
 * A Mini Accumulo cluster will start the following services: Zookeeper Master Monitor GC 2 - TServers Then an interactive shell will start and the cluster will
 * run until the shell is closed
 */
public class StandaloneTestMAC {

  public static void main(String[] args) {

    String host = args[0];
    String pythonExe =null;
    String pythonTestDir = null;
    String shardObjectPath = null;
    String extraJar = null;
	if (args.length > 1){
    pythonExe = args[1];
    pythonTestDir = args[2];
    shardObjectPath = args[3];
    extraJar = args[4];
    }

    // tests expected to accept the following arument
    // -i instance -z zookeeper -u user -p password -t table -s so path

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
      String prt = "2181"; // Integer.valueOf(new Random().nextInt(63000) + 1024).toString();
      int zookeeperPort = Integer.parseInt(System.getProperty("zookeeperPort", prt));
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
        try (Socket ignored = new Socket(host, zookeeperPort)) {
          throw new RuntimeException("Zookeeper can't bind to port already in use: " + zookeeperPort);
        } catch (IOException available) {
          config.setZooKeeperPort(zookeeperPort);
        }
      }

      Map<String,String> siteConfig = config.getSiteConfig();
      final int monitorPort = Integer.parseInt(System.getProperty("monitorPort", "9995"));
      try (Socket ignored = new Socket(host, monitorPort)) {
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

      if (extraJar != null) {
        FileUtils.copyFileToDirectory(new File(extraJar), new File(config.getDir(), "lib"));
      }
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
      boolean startShell = false;

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

      if (null != pythonExe) {
        System.out.println("Running tests.....");

        File folder = new File(pythonTestDir);

        File[] files = folder.listFiles();

        int statusCode = 0;
        for (File file : files) {
          if (file.getName().endsWith(".py")) {
            final String table = UUID.randomUUID().toString().replaceAll("-", "");
            System.out.println("Running " + pythonExe + " " + file.getAbsolutePath() + " -i " + instanceName + " -z localhost:" + zookeeperPort
                    + " -u root -p secret -t " + table + " -s " + shardObjectPath);
            Process p = Runtime.getRuntime().exec(
                    pythonExe + " " + file.getAbsolutePath() + " -i smac -z localhost:" + zookeeperPort + " -u root -p secret -t " + table + " -s "
                            + shardObjectPath);

            BufferedReader input = new BufferedReader(new InputStreamReader(p.getErrorStream()));
            String line;
            while ((line = input.readLine()) != null) {
              System.out.println(line);
            }
            input.close();
            input = new BufferedReader(new InputStreamReader(p.getInputStream()));
            while ((line = input.readLine()) != null) {
              System.out.println(line);
            }
            input.close();
            statusCode |= p.waitFor();
            p.destroy();
            System.out.println("status code " + statusCode);
            if (statusCode > 0) {
              System.exit(statusCode);
            }
          }
        }
      }else{
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
      Files.walkFileTree(dir, new SimpleFileVisitor<Path>() {
        @Override
        public FileVisitResult visitFile(Path file, BasicFileAttributes attrs) throws IOException {
          Files.delete(file);
          return FileVisitResult.CONTINUE;
        }

        @Override
        public FileVisitResult postVisitDirectory(Path dir, IOException exc) throws IOException {
          Files.delete(dir);
          return FileVisitResult.CONTINUE;
        }
      });
    } catch (IOException e) {
      // TODO Auto-generated catch block
      e.printStackTrace();
    }
  }
}
