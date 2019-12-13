package org.poma.accumulo;


import org.apache.commons.io.FilenameUtils;

import java.io.BufferedInputStream;
import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.URL;
import java.security.CodeSource;
import java.security.DigestInputStream;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.List;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.regex.Pattern;
import java.util.zip.Adler32;
import java.util.zip.CheckedInputStream;
import java.util.zip.Checksum;
import java.util.zip.ZipEntry;
import java.util.zip.ZipInputStream;

public class JNILoader {

    private static AtomicBoolean loading = new AtomicBoolean(false);
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
        filenames = new String[] {"pysharkbite-iterators"};

        boolean ret = true;

        for (String file : filenames) {
            ret &= extractAndLoadLibraryFile(dbrNativeLibraryPath, file,libraryname, tempFolder);
        }

        return ret;
    }

private static Collection<String> getNativeObjects(List<String> extensions) throws IOException {
    CodeSource src = DSLIterator.class.getProtectionDomain().getCodeSource();
    List<String> dynObjects = new ArrayList<String>();

    if( src != null ) {
        URL jar = src.getLocation();
        ZipInputStream zip = new ZipInputStream( jar.openStream());
        ZipEntry ze = null;

        while( ( ze = zip.getNextEntry() ) != null ) {
            final String entryName = ze.getName();
            final String extension = FilenameUtils.getExtension(entryName);
            if( extensions.contains(extension)) {
                dynObjects.add( entryName  );
            }
        }

    }
    return dynObjects;
}
    public static boolean isLoaded(){
        return loaded.get();
    }

    public static boolean isLoading(){
        return loading.get();
    }

    public static boolean load() throws Exception {
        loading.set(true);
        String dbrNativeLibraryPath = "";

        List<String> extensions = new ArrayList<>();
        extensions.add("so");
        extensions.add("dylib");
        Collection<String> sos = getNativeObjects(extensions);


        // Temporary library folder
        String tempFolder = new File(System.getProperty("java.io.tmpdir")).getAbsolutePath();

        for(String lib : sos) {
            loaded.set(extractResourceFiles(dbrNativeLibraryPath, lib, tempFolder));
            if (loaded.get())
                break;
        }
        // Extract resource files
        loading.set(false);
        return loaded.get();
    }

    static String shasum(InputStream input) throws IOException {
        BufferedInputStream in = new BufferedInputStream(input);

        try {
            MessageDigest digest = java.security.MessageDigest.getInstance("SHA-512");
            DigestInputStream digestInputStream = new DigestInputStream(in, digest);
            for (; digestInputStream.read() >= 0;) {

            }
            ByteArrayOutputStream shasum = new ByteArrayOutputStream();
            shasum.write(digest.digest());
            return shasum.toString();
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("MD5 algorithm is not available: " + e);
        } finally {
            in.close();
        }
    }

    private static boolean extractAndLoadLibraryFile(String libFolderForCurrentOS, String prefix,String libraryFileName,
                                                     String targetFolder) {
        if (!libraryFileName.startsWith(prefix))
            return false;
        String nativeLibraryFilePath = libFolderForCurrentOS + "/" + libraryFileName;

        String extractedLibFileName = libraryFileName;
        File extractedLibFile = new File(targetFolder, extractedLibFileName);

        try {
            if (extractedLibFile.exists()) {
                Adler32 adlerChecksum = new Adler32();
                final long localsum = new CheckedInputStream(
                        DSLIterator.class.getResourceAsStream(nativeLibraryFilePath),
                        adlerChecksum).getChecksum().getValue();
                final long existingFileSum =
                        new CheckedInputStream(new FileInputStream(extractedLibFile),adlerChecksum).getChecksum().getValue();

                if (existingFileSum  != localsum) {
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