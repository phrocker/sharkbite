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
#include "jni/Iterators.h"
#include "jni/PythonIterator.h"
#include "jni/AccumuloRange.h"
#include "jni/JVMLoader.h"
#include "jni/JNIUtil.h"
#include <pybind11/embed.h>
#include <pybind11/pybind11.h>
#include <pybind11/stl.h>
#include <pybind11/functional.h>

#ifdef __cplusplus
extern "C" {
#endif

void rethrow_cpp_exception_as_java_exception(JNIEnv *env) {
  try {
    throw;  // This allows to determine the type of the exception
  } catch (const std::bad_alloc &e) {
    jclass jc = env->FindClass("java/lang/OutOfMemoryError");
    if (jc)
      env->ThrowNew(jc, e.what());
  } catch (const std::ios_base::failure &e) {
    jclass jc = env->FindClass("java/io/IOException");
    if (jc)
      env->ThrowNew(jc, e.what());
  } catch (const std::exception &e) {
    /* unknown exception (may derive from std::exception) */
    jclass jc = env->FindClass("java/lang/Exception");
    if (jc)
      env->ThrowNew(jc, e.what());
  } catch (...) {
    /* Oops I missed identifying this exception! */
    jclass jc = env->FindClass("java/lang/Exception");
    if (jc)
      env->ThrowNew(jc, "Unidentified exception => "
                    "Improve rethrow_cpp_exception_as_java_exception()");
  }
}

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_setType(JNIEnv *env, jobject me, jstring type) {
  auto typestr = JniStringToUTF(env, type);
  cclient::jni::DSLIterator *ptr = nullptr;
  if (typestr == "Python") {
    ptr = new cclient::jni::python::PythonIterator();
  }
  cclient::jni::JVMLoader::getInstance()->setPtr(env, me, ptr);
}

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_setDSL(JNIEnv *env, jobject me, jstring dsl) {
  const std::string dslStr = JniStringToUTF(env, dsl);
  cclient::jni::DSLIterator *itr = cclient::jni::JVMLoader::getPtr<cclient::jni::DSLIterator>(env, me);
  if (nullptr != itr) {
    try {
      std::cout << "Received DSL" << dslStr << std::endl;
      itr->setDSL(dslStr);
    } catch(...) {
      rethrow_cpp_exception_as_java_exception(env);
    }
  }
}

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_init(JNIEnv *env, jobject me, jobject map) {

}

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_seek(JNIEnv *env, jobject me, jobject skvi, jobject range) {
  cclient::jni::DSLIterator *itr = cclient::jni::JVMLoader::getPtr<cclient::jni::DSLIterator>(env, me);
  THROW_IF_NULL(itr, env, "DSL Iterator must be defined");

  try {
    cclient::jni::AccumuloIterator acciter(env, skvi);
    itr->setIter(&acciter);
    cclient::jni::AccumuloRange rng(env,range);
    THROW_IF( !rng.init(env) , env, "Range is null");
    itr->callSeek(rng.getRange());

    if (itr->callHasTop()) {
      auto top = itr->getTopKey();
      acciter.setTopKey(env,me,top);
      auto topv = itr->getTopValue();
      if ( !acciter.setTopValue(env,me,topv) ) {
        throw std::runtime_error("cannot set top value");
      }
    }

  } catch(...) {
    rethrow_cpp_exception_as_java_exception(env);
  }

}

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_getNextKey(JNIEnv *env, jobject me, jobject skvi) {

  cclient::jni::DSLIterator *itr = cclient::jni::JVMLoader::getPtr<cclient::jni::DSLIterator>(env, me);
  THROW_IF_NULL(itr, env, "DSL Iterator must be defined");

  auto exc = env->FindClass("java/lang/Exception");

  try {
    cclient::jni::AccumuloIterator acciter(env, skvi);
    itr->setIter(&acciter);
    itr->callNext();

    if (itr->callHasTop()) {
      auto top = itr->getTopKey();
      acciter.setTopKey(env,me,top);
      auto topv = itr->getTopValue();
      if ( !acciter.setTopValue(env,me,topv) ) {
        throw std::runtime_error("cannot set top value");
      }
    }
  } catch(...) {
    std::cout << "exception " << std::endl;
    rethrow_cpp_exception_as_java_exception(env);
  }
}

#ifdef __cplusplus
}
#endif
