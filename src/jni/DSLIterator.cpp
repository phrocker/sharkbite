/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "jni/DSLIterator.h"
//#include "jni/Iterators.h"
//#include "jni/AccumuloRange.h"
#include "data/constructs/rfile/RFileOperations.h"
#include "data/iterators/DeletingMultiIterator.h"
#include "data/streaming/accumulo/KeyValueIterator.h"
#include "jni/JNIUtil.h"
#include "jni/JVMLoader.h"
#include "utils/Uri.h"

#ifdef __cplusplus
extern "C" {
#endif

void rethrow_cpp_exception_as_java_exception(JNIEnv *env) {
  try {
    throw;  // This allows to determine the type of the exception
  } catch (const std::bad_alloc &e) {
    jclass jc = env->FindClass("java/lang/OutOfMemoryError");
    if (jc) env->ThrowNew(jc, e.what());
  } catch (const std::ios_base::failure &e) {
    jclass jc = env->FindClass("java/io/IOException");
    if (jc) env->ThrowNew(jc, e.what());
  } catch (const std::exception &e) {
    /* unknown exception (may derive from std::exception) */
    jclass jc = env->FindClass("java/lang/Exception");
    if (jc) env->ThrowNew(jc, e.what());
  } catch (...) {
    /* Oops I missed identifying this exception! */
    jclass jc = env->FindClass("java/lang/Exception");
    if (jc)
      env->ThrowNew(jc,
                    "Unidentified exception => "
                    "Improve rethrow_cpp_exception_as_java_exception()");
  }
}
JNIEXPORT void JNICALL
Java_org_apache_accumulo_tserver_tablet_NativeCompactor_callCompact(
    JNIEnv *env, jobject me, jstring output_file, jobject arraylist_files,
    jobject longadder_resulted, jobject longadder_total,
    jobject longadder_filesize) {
  try {
    env->ExceptionClear();
    // retrieve the java.util.List interface class
    jclass cList = env->FindClass("java/util/List");
    cclient::jni::ThrowIf(env);
    jclass jadderClass =
        env->FindClass("java/util/concurrent/atomic/LongAdder");
    cclient::jni::ThrowIf(env);
    // retrieve the size and the get method
    jmethodID mSize = env->GetMethodID(cList, "size", "()I");
    jmethodID mGet = env->GetMethodID(cList, "get", "(I)Ljava/lang/Object;");
    jmethodID mAdd = env->GetMethodID(jadderClass, "add", "(J)V");
    cclient::jni::ThrowIf(env);
    THROW_IF_NULL(mSize, env, "Could not get size function");
    THROW_IF_NULL(mGet, env, "Could not obtain get function");
    THROW_IF_NULL(mAdd, env, "Could not get add function");
    // get the size of the list
    jint size = env->CallIntMethod(arraylist_files, mSize);
    cclient::jni::ThrowIf(env);
    std::vector<std::string> rfiles;

    auto file = JniStringToUTF(env, output_file);
    cclient::jni::ThrowIf(env);

    bool onHdfs = false;
    if (file.find("hdfs://") != std::string::npos) {
      onHdfs = true;
    }

    // walk through and fill the vector
    for (jint i = 0; i < size; i++) {
      jstring strObj = (jstring)env->CallObjectMethod(arraylist_files, mGet, i);
      cclient::jni::ThrowIf(env);
      auto rfile = JniStringToUTF(env, strObj);
      if (rfile.find("hdfs://") == std::string::npos && onHdfs) {
        try {
          utils::Uri uri(file);
          rfile =
              "hdfs://" + uri.host() + ":" + std::to_string(uri.port()) + rfile;
        } catch (...) {
        }
      }
      rfiles.push_back(rfile);
    }
    auto outStream = cclient::data::RFileOperations::write(file, 32 * 1024);
    std::shared_ptr<cclient::data::streams::KeyValueIterator> multi_iter =
        cclient::data::RFileOperations::openManySequential(rfiles, 0, true,
                                                           false);
    std::vector<std::string> cf;
    cclient::data::Range rng;

    cclient::data::security::Authorizations auths;

    cclient::data::streams::StreamSeekable seekable(rng, cf, auths, false);
    multi_iter->relocate(&seekable);
    long count = 0;
    uint64_t total_size = 0;
    outStream->addLocalityGroup();
    while (multi_iter->hasNext()) {
      outStream->append(std::make_shared<cclient::data::KeyValue>(
          multi_iter->getTopKey(), multi_iter->getTopValue()));

      multi_iter->next();
      count++;
      if ((count % 10000) == 0) {
        env->CallVoidMethod(longadder_resulted, mAdd, count);
        env->CallVoidMethod(longadder_total, mAdd, count);
        count = 0;
      }
    }
    outStream->close();
    outStream = nullptr;

    // auto end = chrono::steady_clock::now();

    env->CallVoidMethod(longadder_resulted, mAdd, count);

    env->CallVoidMethod(longadder_total, mAdd,
                        count + multi_iter->getEntriesFiltered());

    auto res_file_size = cclient::data::RFileOperations::filesize(file.c_str());

    env->CallVoidMethod(longadder_filesize, mAdd, res_file_size);
  } catch (...) {
    rethrow_cpp_exception_as_java_exception(env);
  }
}

#ifdef __cplusplus
}
#endif
