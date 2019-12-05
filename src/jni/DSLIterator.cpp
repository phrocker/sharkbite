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
#include "jni/JVMLoader.h"
#include "jni/JNIUtil.h"

#ifdef __cplusplus
extern "C" {
#endif

JNIEXPORT void JNICALL  Java_org_poma_accumulo_DSLIterator_setType(JNIEnv *env, jobject me, jstring type) {
  auto typestr = JniStringToUTF(env, type);
  cclient::jni::DSLIterator *ptr = nullptr;
  if (typestr == "Python") {
    ptr = new cclient::jni::python::PythonIterator();
  }
  cclient::jni::JVMLoader::getInstance()->setPtr(env, me, ptr);
}

JNIEXPORT void JNICALL  Java_org_poma_accumulo_DSLIterator_setDSL(JNIEnv *env, jobject me, jstring dsl) {
  const std::string dslStr = JniStringToUTF(env, dsl);
  cclient::jni::DSLIterator *itr = cclient::jni::JVMLoader::getPtr<cclient::jni::DSLIterator>(env, me);
  if (nullptr != itr) {
    itr->setDSL(dslStr);
  }
}

JNIEXPORT void JNICALL  Java_org_poma_accumulo_DSLIterator_init(JNIEnv *env, jobject me, jobject map) {

}

JNIEXPORT jobject JNICALL  Java_org_poma_accumulo_DSLIterator_getNextKey(JNIEnv *env, jobject me, jobject skvi) {
  cclient::jni::AccumuloIterator acciter(env, skvi);
  cclient::jni::DSLIterator *itr = cclient::jni::JVMLoader::getPtr<cclient::jni::DSLIterator>(env, me);
  THROW_IF_NULL(itr, env, "DSL Iterator must be defined");
  itr->setIter(&acciter);
  std::cout << "a" << std::endl;
  itr->callNext();
  std::cout << "b" << std::endl;
  if (itr->hasTop()) {
    std::cout << "c" << std::endl;
    itr->callGetTopKey();
    itr->callgetTopValue();
    std::cout << "dd" << std::endl;
    auto key = itr->getTopKey();
    auto value = itr->getTopValue();
    std::cout << "e" << std::endl;
  } else {
    std::cout << "d" << std::endl;
    return nullptr;
  }

}

#ifdef __cplusplus
}
#endif
