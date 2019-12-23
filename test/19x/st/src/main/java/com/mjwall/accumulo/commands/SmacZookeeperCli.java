package com.mjwall.accumulo.commands;

import com.mjwall.accumulo.config.RunningEnv;
import org.apache.zookeeper.KeeperException;
import org.apache.zookeeper.ZooKeeperMain;

import java.io.FileInputStream;
import java.io.IOException;
import java.nio.file.Files;
import java.util.Properties;

import static com.mjwall.accumulo.config.RunningEnv.RUNNING_ENV_FILE;

public class SmacZookeeperCli {

  private final String zookeepers;

  public SmacZookeeperCli() {
    if (Files.exists(RUNNING_ENV_FILE)) {
      Properties runningEnv = new Properties();
      try {
        runningEnv.load(new FileInputStream(RUNNING_ENV_FILE.toFile()));
        this.zookeepers = runningEnv.getProperty(RunningEnv.ZOOKEEPERS_PROP);
      } catch (IOException e) {
        throw new RuntimeException("Could not load " + RUNNING_ENV_FILE);
      }
    } else {
      throw new RuntimeException("Running smac env not found: " + RUNNING_ENV_FILE);
    }
  }

  public void start() {
    String[] cliArgs = new String[] {"-server", this.zookeepers};
    try {
      ZooKeeperMain.main(cliArgs);
    } catch (InterruptedException e) {
      e.printStackTrace();
    } catch (KeeperException e) {
      e.printStackTrace();
    } catch (IOException e) {
      e.printStackTrace();
    }
  }

  public static void main(String[] args) {
    new SmacZookeeperCli().start();
  }
}
