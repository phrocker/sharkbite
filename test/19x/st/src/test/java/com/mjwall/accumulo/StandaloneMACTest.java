package com.mjwall.accumulo;

import org.junit.After;
import org.junit.Test;

import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.net.Socket;

import static org.junit.Assert.assertEquals;

public class StandaloneMACTest {

  private final int SHUTDOWN_PORT = 4446;

  @After
  public void tearDown() {
    System.clearProperty("startShell");
    System.clearProperty("shutdownPort");

    try {
      Socket socket = new Socket("localhost", SHUTDOWN_PORT);
      BufferedReader in = new BufferedReader(new InputStreamReader(socket.getInputStream()));
      System.out.println(in.readLine());
      in.close();
      socket.close();
    } catch (IOException e) {
      e.printStackTrace();
    }
  }

  // @Test
  public void testSMACRuns() {
    Runnable r = new Runnable() {
      @Override
      public void run() {
        System.out.println("Starting SMAC in the back");
        System.setProperty("startShell", "false");
        System.setProperty("shutdownPort", Integer.toString(SHUTDOWN_PORT));
        StandaloneMAC.main(new String[] {});
      }
    };

    Thread t = new Thread(r);
    t.start();

    try {
      Thread.sleep(10000);
    } catch (InterruptedException e) {
      e.printStackTrace();
    }
    assertEquals("A", "B");
  }

}
