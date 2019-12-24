package com.mjwall.accumulo.commands;

import com.mjwall.accumulo.config.RunningEnv;
import org.apache.accumulo.shell.Shell;

import java.io.FileInputStream;
import java.io.IOException;
import java.net.ServerSocket;
import java.nio.file.Files;
import java.util.Properties;

import static com.mjwall.accumulo.config.RunningEnv.RUNNING_ENV_FILE;

public class SmacAccumuloShell {

  private final String username;
  private final String password;
  private final String instanceName;
  private final String zookeepers;
  private final int shutdownPort;

  public SmacAccumuloShell() {
    if (Files.exists(RUNNING_ENV_FILE)) {
      Properties runningEnv = new Properties();
      try {
        runningEnv.load(new FileInputStream(RUNNING_ENV_FILE.toFile()));
        username = runningEnv.getProperty(RunningEnv.ROOT_USERNAME_PROP);
        password = runningEnv.getProperty(RunningEnv.ROOT_PASSWORD_PROP);
        instanceName = runningEnv.getProperty(RunningEnv.INSTANCE_NAME_PROP);
        zookeepers = runningEnv.getProperty(RunningEnv.ZOOKEEPERS_PROP);
        shutdownPort = Integer.parseInt(runningEnv.getProperty(RunningEnv.SHUTDOWN_PORT_PROP));
      } catch (IOException e) {
        throw new RuntimeException("Could not load " + RUNNING_ENV_FILE);
      }
    } else {
      throw new RuntimeException("Running smac env not found: " + RUNNING_ENV_FILE);
    }
  }

  public void startWithShutdown() throws IOException {
    Runnable shutdownTask = new Runnable() {
      @Override
      public void run() {
        try {
          ServerSocket serverSocket = new ServerSocket(shutdownPort);
          serverSocket.accept().close(); // this will block until the port is hit
          // none of this worked, so lets' get out
          System.exit(0);
          // shell.setExit(true);
          // shell.execCommand("users", false, false);
          // shell.shutdown();
          // System.out.println("Shutdown port was hit, see you");
          // jstack stuck at
          // at jline.console.ConsoleReader.readCharacter(ConsoleReader.java:2038)
          // at jline.console.ConsoleReader.readLine(ConsoleReader.java:2242)
          // at jline.console.ConsoleReader.readLine(ConsoleReader.java:2162)
          // at jline.console.ConsoleReader.readLine(ConsoleReader.java:2150)
          // at org.apache.accumulo.shell.Shell.start(Shell.java:700)
          // at com.mjwall.accumulo.commands.SmacAccumuloShell.start(SmacAccumuloShell.java:64)
          // at com.mjwall.accumulo.StandaloneMAC.main(StandaloneMAC.java:159)
        } catch (IOException e) {
          System.err.println("Unable to process client request");
          e.printStackTrace();
        }
      }
    };
    Thread shutdownThread = new Thread(shutdownTask);
    shutdownThread.setDaemon(true); // so if exit is typed into the shell, the shell will exit
    shutdownThread.start();
    start();
  }

  public void start() throws IOException {
    String[] shellArgs = new String[] {"-u", this.username, "-p", this.password, "-zi", this.instanceName, "-zh", this.zookeepers};
    Shell shell = new Shell();
    shell.config(shellArgs);
    shell.start(); // this is gonna block til you exit
    shell.shutdown();
  }

  public static void main(String[] args) throws IOException {
    new SmacAccumuloShell().start();
  }

}
