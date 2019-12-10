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
#include "AccumuloRange.h"
#include "AccumuloKey.h"
#include "JavaException.h"

namespace cclient {
namespace jni {




class AccumuloIterator {
  jobject iter;
  JNIEnv *env;
  jclass iterClass;
  jmethodID nextMethod;
  jmethodID hasTopMethod;
  jmethodID getTopKeyMethod;
  jmethodID getTopValueMethod;
  jmethodID seekMethod;

  //jmethodID setTopKeyMethod;
  //jmethodID setTopValueMethod;
  std::shared_ptr<cclient::data::Key> key;
  std::shared_ptr<cclient::data::Value> value;
 public:
  AccumuloIterator(JNIEnv *env, jobject itr)
      :
      env(env),
      iter(itr) {
    iterClass = env->GetObjectClass(iter);
    nextMethod = env->GetMethodID(iterClass, "next", "()V");
    hasTopMethod = env->GetMethodID(iterClass, "hasTop", "()Z");
    getTopKeyMethod = env->GetMethodID(iterClass, "getTopKey", "()Lorg/apache/accumulo/core/data/Key;");

    ///setTopValueMethod = env->GetMethodID(iterClass, "setTopValue", "(Lorg/apache/accumulo/core/data/Value;)V");
    getTopValueMethod = env->GetMethodID(iterClass, "getTopValue", "()Lorg/apache/accumulo/core/data/Value;");
    seekMethod = env->GetMethodID(iterClass, "seek", "(Lorg/apache/accumulo/core/data/Range;)V");
  }

  bool hasTop() {
    auto jbool = env->CallBooleanMethod(iter, hasTopMethod);
    if (env->ExceptionCheck()) {
      return false;
    }
    return (jbool == JNI_TRUE);
  }

  void next() {
    key = nullptr;
    value = nullptr;
    /**
     * Call next on the underlying iterator.
     */
    env->CallVoidMethod(iter, nextMethod);
    if (env->ExceptionCheck()) {
      return;
    }
    if (hasTop()) {
      auto jkey = env->CallObjectMethod(iter, getTopKeyMethod);
      if (env->ExceptionCheck()) {
        return;
      }
      AccumuloKey keyTransfer(env, jkey);
      if (!keyTransfer.init(env)){
        return;
      }
      key = std::make_shared<cclient::data::Key>();
      key->setRow(keyTransfer.getRow());
      key->setColFamily(keyTransfer.getCf());
      key->setColQualifier(keyTransfer.getCq());
      key->setColVisibility(keyTransfer.getCv());
      key->setTimeStamp(keyTransfer.getTimeStamp());
      auto jvalue = env->CallObjectMethod(iter, getTopValueMethod);
      if (env->ExceptionCheck()) {
        return;
      }



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

  void seek(const std::shared_ptr<cclient::data::Range> &rng) {

      key = nullptr;
      value = nullptr;
      /**
       * Call next on the underlying iterator.
       */
      AccumuloRange range(rng);
      env->CallVoidMethod(iter, seekMethod, range.getAccumuloRange(env));
      if (env->ExceptionCheck()) {
              return;
            }

      if (hasTop()) {

            auto jkey = env->CallObjectMethod(iter, getTopKeyMethod);
            if (env->ExceptionCheck()) {
              return;
            }
            AccumuloKey keyTransfer(env, jkey);
            if (!keyTransfer.init(env)){

              return;
            }
            key = std::make_shared<cclient::data::Key>();
            key->setRow(keyTransfer.getRow());
            key->setColFamily(keyTransfer.getCf());
            key->setColQualifier(keyTransfer.getCq());
            key->setColVisibility(keyTransfer.getCv());
            key->setTimeStamp(keyTransfer.getTimeStamp());
            auto jvalue = env->CallObjectMethod(iter, getTopValueMethod);
            if (env->ExceptionCheck()) {
              return;
            }


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


  std::shared_ptr<cclient::data::Key> getTopKey(){
    return key;
  }
   std::shared_ptr<cclient::data::Value> getTopValue(){
     return value;
   }
  bool setTopKey(JNIEnv *env,jobject itr) {
    auto clz= env->GetObjectClass(itr);
    auto setTopKeyMethod = env->GetMethodID(clz, "setTopKey", "(Lorg/apache/accumulo/core/data/Key;)V");

    if (!key)
      return false;
    //return key;
    AccumuloKey keyTransfer(key);
        env->CallVoidMethod(itr, setTopKeyMethod,keyTransfer.getAccumuloKey(env));
          if (env->ExceptionCheck()) {

            return false;
          }
          return true;
  }
  /*
  bool setTopValue(JNIEnv *env) {
    //return value;
     env->CallVoidMethod(iter, setTopValueMethod,nullptr);
     if (env->ExceptionCheck()) {
                 return false;
               }
     return true;
  }*/

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

  virtual bool callHasTop() = 0;

  virtual void callGetTopKey() = 0;

  virtual void callGetTopValue() = 0;

  virtual void callSeek(const std::shared_ptr<cclient::data::Range> &range) = 0;

  virtual void seek(const std::shared_ptr<cclient::data::Range> &range){
    iter->seek(range);
  }

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

  virtual bool hasTop() = 0;

  virtual bool iteratorHasTop() {
    return iter->hasTop();
  }

  virtual void next() {
    iter->next();
  }
};

} /** jni **/
} /** cclient **/

#endif /* _ITERATORS_ */
