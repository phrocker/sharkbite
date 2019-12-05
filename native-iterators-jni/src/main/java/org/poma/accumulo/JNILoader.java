package org.poma.accumulo;


import java.io.BufferedInputStream;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.security.DigestInputStream;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.concurrent.atomic.AtomicBoolean;

public class JNILoader {

    private static AtomicBoolean loaded = new AtomicBoolean(false);

    private static synchronized boolean loadNativeLibrary(String path, String name) {
        File libPath = new File(path, name);
        if (libPath.exists()) {
            try {
                System.load(new File(path, name).getAbsolutePath());
                return true;
            } catch (UnsatisfiedLinkError e) {
                System.err.println(e);
                return false;
            }

        } else
            return false;
    }

    private static boolean extractResourceFiles(String dbrNativeLibraryPath, String libraryname,
                                                String tempFolder) throws IOException {
        String[] filenames = null;
        filenames = new String[] {"pysharkbite.cpython-37m-x86_64-linux-gnu.so"};

        boolean ret = true;

        for (String file : filenames) {
            ret &= extractAndLoadLibraryFile(dbrNativeLibraryPath, file, tempFolder);
        }

        return ret;
    }

    public static boolean isLoaded(){
        return loaded.get();
    }

    public static boolean load() throws Exception {

        //System.loadLibrary("python");
        String libraryname = "pysharkbite.cpython-37m-x86_64-linux-gnu.so";

        ///org/poma/accumulo/native/linux
        String dbrNativeLibraryPath = "";

        if (DSLIterator.class.getResource(dbrNativeLibraryPath + "/" + libraryname) == null) {
            throw new Exception("Error loading native library: " + dbrNativeLibraryPath + "/" + libraryname);
        }

        // Temporary library folder
        String tempFolder = new File(System.getProperty("java.io.tmpdir")).getAbsolutePath();

        loaded.set(extractResourceFiles(dbrNativeLibraryPath, libraryname, tempFolder));
        // Extract resource files
        return loaded.get();
    }

    static String shasum(InputStream input) throws IOException {
        BufferedInputStream in = new BufferedInputStream(input);

        try {
            MessageDigest digest = java.security.MessageDigest.getInstance("SHA-256");
            DigestInputStream digestInputStream = new DigestInputStream(in, digest);
            for (; digestInputStream.read() >= 0;) {

            }
            ByteArrayOutputStream md5out = new ByteArrayOutputStream();
            md5out.write(digest.digest());
            return md5out.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("MD5 algorithm is not available: " + e);
        } finally {
            in.close();
        }
    }

    private static boolean extractAndLoadLibraryFile(String libFolderForCurrentOS, String libraryFileName,
                                                     String targetFolder) {
        String nativeLibraryFilePath = libFolderForCurrentOS + "/" + libraryFileName;

        String extractedLibFileName = libraryFileName;
        File extractedLibFile = new File(targetFolder, extractedLibFileName);

        try {
            if (extractedLibFile.exists()) {
                // test shasum value
                String shasum1 = shasum(DSLIterator.class.getResourceAsStream(nativeLibraryFilePath));
                String shasum2 = shasum(new FileInputStream(extractedLibFile));

                if (shasum1.equals(shasum2)) {
                    return loadNativeLibrary(targetFolder, extractedLibFileName);
                } else {
                    // remove old native library file
                    boolean deletionSucceeded = extractedLibFile.delete();
                    if (!deletionSucceeded) {
                        throw new IOException(
                                "failed to remove existing native library file: " + extractedLibFile.getAbsolutePath());
                    }
                }
            }

            // Extract file into the current directory
            InputStream reader = DSLIterator.class.getResourceAsStream(nativeLibraryFilePath);
            FileOutputStream writer = new FileOutputStream(extractedLibFile);
            byte[] buffer = new byte[1024];
            int bytesRead = 0;
            while ((bytesRead = reader.read(buffer)) != -1) {
                writer.write(buffer, 0, bytesRead);
            }

            writer.close();
            reader.close();

            if (!System.getProperty("os.name").contains("Windows")) {
                try {
                    Runtime.getRuntime().exec(new String[] { "chmod", "755", extractedLibFile.getAbsolutePath() })
                            .waitFor();
                } catch (Throwable e) {
                }
            }

            return loadNativeLibrary(targetFolder, extractedLibFileName);
        } catch (IOException e) {
            System.err.println(e.getMessage());
            return false;
        }

    }
}