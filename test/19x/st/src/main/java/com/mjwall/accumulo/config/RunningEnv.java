package com.mjwall.accumulo.config;

import org.apache.accumulo.minicluster.impl.MiniAccumuloClusterImpl;

import java.io.File;
import java.io.IOException;
import java.lang.management.ManagementFactory;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;

/**
 * Represents a Running SMAC environment
 */
public class RunningEnv {

  public final static String INSTANCE_NAME_PROP = "INSTANCE_NAME";
  public final static String ROOT_USERNAME_PROP = "ROOT_USERNAME";
  public final static String ROOT_PASSWORD_PROP = "ROOT_PASSWORD";
  public final static String TEMP_MINI_DIR_PROP = "TEMP_MINI_DIR";
  public final static String NUM_TSERVERS_PROP = "NUM_TSERVERS";
  public final static String ZOOKEEPERS_PROP = "ZOOKEEPERS";
  public final static String REUSE_TEMP_MINI_DIR_PROP = "REUSE_TEMP_MINI_DIR";
  public final static String MONITOR_URL_PROP = "MONITOR_URL";
  public final static String INIT_SCRIPT_PROP = "INIT_SCRIPT";
  public final static String START_SHELL_PROP = "START_SHELL";
  public final static String SET_JDWP_PROP = "SET_JDWP";
  public final static String SHUTDOWN_PORT_PROP = "SHUTDOWN_PORT";
  public final static String RUNNING_PID = "RUNNING_PID";
  public final static String LOG4J_CONF_PROP = "LOG4J_CONF";
  public final static String TSERV_CLIENTPORT_PROP = "TSERV_CLIENTPORT";

  public final static Path RUNNING_ENV_FILE = Paths.get(System.getProperty("user.home") + System.getProperty("file.separator") + ".smac-running-env");

  private final String instanceName;
  private final String rootUsername;
  private final String rootPassword;
  private final File tempMiniDir;
  private final int numTservers;
  private final String zookeepers;
  private final boolean reuseTempMiniDir;
  private final String monitorUrl;
  private final String initScript;
  private final boolean startShell;
  private final boolean setJDWP;
  private final int shutdownPort;
  private final String runningPid;
  private final String log4jConfig;
  private final String tservClientport;

  public RunningEnv(MiniAccumuloClusterImpl cluster, boolean reuseTempMiniDir, String monitorUrl, String initScript, boolean startShell, boolean setJDWP,
      int shutdownPort, String log4jConfig, String tserverClientport) {
    this.instanceName = cluster.getInstanceName();
    this.rootUsername = cluster.getConfig().getRootUserName();
    this.rootPassword = cluster.getConfig().getRootPassword();
    this.tempMiniDir = cluster.getConfig().getDir();
    this.numTservers = cluster.getConfig().getNumTservers();
    this.zookeepers = cluster.getZooKeepers();
    this.reuseTempMiniDir = reuseTempMiniDir;
    this.monitorUrl = monitorUrl;
    this.initScript = initScript;
    this.startShell = startShell;
    this.setJDWP = setJDWP;
    this.shutdownPort = shutdownPort;
    // killing this pid leaves orphaned processes running, don't do that
    // only used to ensure that the ~/.smac-running-env is current
    this.runningPid = ManagementFactory.getRuntimeMXBean().getName().split("@")[0];
    this.log4jConfig = log4jConfig;
    this.tservClientport = tserverClientport;
  }

  // TODO: constructor reading from file

  public String getRunningPid() {
    return this.runningPid;
  }

  public void toStdOut() {
    System.out.println("InstanceName      : " + this.instanceName);
    System.out.println("Root username     : " + this.rootUsername);
    System.out.println("Root password     : " + this.rootPassword);
    System.out.println("Temp mini dir     : " + this.tempMiniDir);
    System.out.println("Num tservers      : " + this.numTservers);
    System.out.println("Zookeepers        : " + this.zookeepers);
    System.out.println("Reuse tempMiniDir : " + this.reuseTempMiniDir);
    System.out.println("Monitor url       : " + this.monitorUrl);
    System.out.println("Init script       : " + this.initScript);
    System.out.println("Start shell       : " + this.startShell);
    System.out.println("Set JDWP          : " + this.reuseTempMiniDir);
    System.out.println("Shutdown port     : " + this.shutdownPort);
    System.out.println("Running pid       : " + this.runningPid);
    System.out.println("Log4j config      : " + this.log4jConfig);
    System.out.println("Tserv Client port : " + this.tservClientport);
  }

  public void writeEnvFile() {
    StringBuilder sb = new StringBuilder();
    sb.append(INSTANCE_NAME_PROP).append("=").append(this.instanceName).append("\n");
    sb.append(ROOT_USERNAME_PROP).append("=").append(this.rootUsername).append("\n");
    sb.append(ROOT_PASSWORD_PROP).append("=").append(this.rootPassword).append("\n");
    sb.append(TEMP_MINI_DIR_PROP).append("=").append(this.tempMiniDir).append("\n");
    sb.append(NUM_TSERVERS_PROP).append("=").append(this.numTservers).append("\n");
    sb.append(ZOOKEEPERS_PROP).append("=").append(this.zookeepers).append("\n");
    sb.append(REUSE_TEMP_MINI_DIR_PROP).append("=").append(this.reuseTempMiniDir).append("\n");
    sb.append(MONITOR_URL_PROP).append("=").append(this.monitorUrl).append("\n");
    sb.append(INIT_SCRIPT_PROP).append("=").append(this.initScript).append("\n");
    sb.append(START_SHELL_PROP).append("=").append(this.startShell).append("\n");
    sb.append(SET_JDWP_PROP).append("=").append(this.setJDWP).append("\n");
    sb.append(SHUTDOWN_PORT_PROP).append("=").append(this.shutdownPort).append("\n");
    sb.append(RUNNING_PID).append("=").append(this.runningPid).append("\n");
    sb.append(LOG4J_CONF_PROP).append("=").append(this.log4jConfig).append("\n");
    sb.append(TSERV_CLIENTPORT_PROP).append("=").append(this.tservClientport).append("\n");
    if (Files.exists(RUNNING_ENV_FILE)) {
      System.err.println("Running SMAC env file exists, overwriting at " + RUNNING_ENV_FILE);
      try {
        Files.delete(RUNNING_ENV_FILE);
      } catch (IOException e) {
        e.printStackTrace();
      }
    }
    try {
      System.out.println("Writing SMAC env to " + RUNNING_ENV_FILE.toAbsolutePath().toString());
      Files.write(RUNNING_ENV_FILE, sb.toString().getBytes());
    } catch (IOException e) {
      e.printStackTrace();
    }
  }
}
