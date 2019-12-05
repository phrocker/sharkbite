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

#ifndef _ITERATORS_
#define _ITERATORS_

#include <jni.h>
#include "data/constructs/Key.h"
#include "data/constructs/value.h"
#include "JNIUtil.h"

namespace cclient {
namespace jni {

class AccumuloKey {
  jobject key;
  JNIEnv *env;
  jclass keyClass;

  std::string row, cf, cq, cv;
  jlong ts = 0;
  bool deleted = false;
 public:
  AccumuloKey(JNIEnv *env, jobject key)
      :
      env(env),
      key(key) {
    keyClass = env->GetObjectClass(key);

    /**
     * load compoonent parts
     */

    auto textClass = env->FindClass("org/apache/hadoop/io/Text");

    auto getRow = env->GetMethodID(keyClass, "getRow", "()Lorg/apache/hadoop/io/Text;");
    auto getColumnFamily = env->GetMethodID(keyClass, "getColumnFamily", "()Lorg/apache/hadoop/io/Text;");
    auto getColumnQualifier = env->GetMethodID(keyClass, "getColumnQualifier", "()Lorg/apache/hadoop/io/Text;");
    auto getColumnVisibility = env->GetMethodID(keyClass, "getColumnVisibility", "()Lorg/apache/hadoop/io/Text;");
    auto getTimestamp = env->GetMethodID(keyClass, "getTimestamp", "()J");

    auto jrow = env->CallObjectMethod(key, getRow);
    auto jcf = env->CallObjectMethod(key, getColumnFamily);
    auto jcq = env->CallObjectMethod(key, getColumnQualifier);
    auto jcv = env->CallObjectMethod(key, getColumnVisibility);
    ts = env->CallLongMethod(key, getTimestamp);

    auto textToString = env->GetMethodID(textClass, "toString", "()Ljava/lang/String;");

    auto jrowstr = (jstring) env->CallObjectMethod(jrow, textToString);
    auto jcfstr = (jstring) env->CallObjectMethod(jcf, textToString);
    auto jcqstr = (jstring) env->CallObjectMethod(jcq, textToString);
    auto jcvstr = (jstring) env->CallObjectMethod(jcv, textToString);

    row = JniStringToUTF(env, jrowstr);
    cf = JniStringToUTF(env, jcfstr);
    cq = JniStringToUTF(env, jcqstr);
    cv = JniStringToUTF(env, jcvstr);

  }

  AccumuloKey(const std::shared_ptr<cclient::data::Key> &key)
      :
      env(nullptr),
      key(nullptr) {

    row = key->getRowStr();
    cf = key->getColFamilyStr();
    cq = key->getColQualifierStr();
    cv = key->getColVisibilityStr();
    ts = key->getTimeStamp();

    keyClass = env->FindClass("org/apache/accumulo/core/data/Key");

  }

  jobject getAccumuloKey(JNIEnv *env) const {

    auto constructor = env->GetMethodID(keyClass, "<init>", "([B[B[B[BJ)V");

    auto jrow = toByteArray(env, row);
    auto jcf = toByteArray(env, cf);
    auto jcq = toByteArray(env, cq);
    auto jcv = toByteArray(env, cv);
    jlong timeStamp = ts;
    auto jkey = env->NewObject(keyClass, constructor, jrow, jcf, jcq, jcv, timeStamp);

    env->DeleteLocalRef(jrow);
    env->DeleteLocalRef(jcf);
    env->DeleteLocalRef(jcq);
    env->DeleteLocalRef(jcv);

    return jkey;

    /**
     *  auto ff = ptr->get();
     jclass mapClass = env->FindClass("java/util/HashMap");
     if (mapClass == nullptr) {
     return nullptr;
     }

     jsize map_len = ff->getAttributes().size();

     jmethodID init = env->GetMethodID(mapClass, "<init>", "(I)V");
     jobject hashMap = env->NewObject(mapClass, init, map_len);

     jmethodID put = env->GetMethodID(mapClass, "put", "(Ljava/lang/Object;Ljava/lang/Object;)Ljava/lang/Object;");

     for (auto kf : ff->getAttributes()) {
     env->CallObjectMethod(hashMap, put, env->NewStringUTF(kf.first.c_str()), env->NewStringUTF(kf.second.c_str()));
     minifi::jni::ThrowIf(env);
     }
     */
    //public Key(byte[] row, byte[] cf, byte[] cq, byte[] cv, long ts, boolean deleted, boolean copy) {
  }

  std::string getRow() const {
    return row;
  }

  std::string getCf() const {
    return cf;
  }

  std::string getCq() const {
    return cq;
  }

  std::string getCv() const {
    return cv;
  }

  long getTimeStamp() const {
    return ts;
  }

};

class AccumuloIterator {
  jobject iter;
  JNIEnv *env;
  jclass iterClass;
  jmethodID nextMethod;
  jmethodID hasTopMethod;
  jmethodID getTopKeyMethod;
  jmethodID getTopValueMethod;
  std::shared_ptr<cclient::data::Key> key;
  std::shared_ptr<cclient::data::Value> value;
 public:
  AccumuloIterator(JNIEnv *env, jobject itr)
      :
      env(env),
      iter(itr) {
    std::cout << "oh hai" << std::endl;
    iterClass = env->GetObjectClass(iter);
    std::cout << "oh hai" << std::endl;
    nextMethod = env->GetMethodID(iterClass, "next", "()V");
    std::cout << "oh hai" << std::endl;
    hasTopMethod = env->GetMethodID(iterClass, "hasTop", "()Z");
    std::cout << "oh hai" << std::endl;
    getTopKeyMethod = env->GetMethodID(iterClass, "getTopKey", "()Lorg/apache/accumulo/core/data/Key;");
    std::cout << "oh hai" << std::endl;
    getTopValueMethod = env->GetMethodID(iterClass, "getTopValue", "()Lorg/apache/accumulo/core/data/Value;");
    std::cout << "oh hai" << std::endl;
  }

  bool hasTop() {
    auto jbool = env->CallBooleanMethod(iter, hasTopMethod);
    return (jbool == JNI_TRUE);
  }

  void next() {
    std::cout << "lolwut" << std::endl;
    key = nullptr;
    value = nullptr;
    /**
     * Call next on the underlying iterator.
     */
    env->CallVoidMethod(iter, nextMethod);
    if (hasTop()) {
      std::cout << "oh mang have top?!" << std::endl;
      auto jkey = env->CallObjectMethod(iter, getTopKeyMethod);

      AccumuloKey keyTransfer(env, jkey);

      key = std::make_shared<cclient::data::Key>();
      key->setRow(keyTransfer.getRow());
      key->setColFamily(keyTransfer.getCf());
      key->setColQualifier(keyTransfer.getCq());
      key->setColVisibility(keyTransfer.getCv());
      key->setTimeStamp(keyTransfer.getTimeStamp());
      auto jvalue = env->CallObjectMethod(iter, getTopValueMethod);
      // do value later;
      value = std::make_shared<cclient::data::Value>();
    }

    /*
     class arraylist = env->FindClass("java/util/ArrayList");
     jmethodID init_method = env->GetMethodID(arraylist, "<init>", "(I)V");
     jmethodID add_method = env->GetMethodID(arraylist, "add", "(Ljava/lang/Object;)Z");
     jobject result = env->NewObject(arraylist, init_method, keys.size());
     for (const auto &s : keys) {
     if (s.second.isTransient()) {
     jstring element = env->NewStringUTF(s.first.c_str());
     env->CallBooleanMethod(result, add_method, element);
     minifi::jni::ThrowIf(env);
     env->DeleteLocalRef(element);
     }
     }
     */
  }

  std::shared_ptr<cclient::data::Key> getTopKey() {
    return key;
  }
  std::shared_ptr<cclient::data::Value> getTopValue() {
    return value;
  }

};

class DSLIterator {
  AccumuloIterator *iter;
 public:
  DSLIterator()
      :
      iter(nullptr) {

  }
  virtual ~DSLIterator() {

  }

  virtual void callNext() = 0;

  virtual void callGetTopKey() = 0;

  virtual void callgetTopValue() = 0;

  virtual void setIter(AccumuloIterator *iter) {
    this->iter = iter;
  }
  virtual void setDSL(const std::string &dsl) = 0;

  virtual std::shared_ptr<cclient::data::Key> getTopKey() {
    return getIteratorTopKey();
  }
  virtual std::shared_ptr<cclient::data::Value> getTopValue() {
    return getIteratorTopValue();
  }

  virtual std::shared_ptr<cclient::data::Key> getIteratorTopKey() {
    return iter->getTopKey();
  }
  virtual std::shared_ptr<cclient::data::Value> getIteratorTopValue() {
    return iter->getTopValue();;
  }

  virtual bool hasTop() {
    return iter->hasTop();
  }

  virtual void next() {
    iter->next();
  }
};

} /** jni **/
} /** cclient **/

#endif /* _ITERATORS_ */
