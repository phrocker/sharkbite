/**
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#ifndef INCLUDE_JNI_ACCUMULORANGE_H_
#define INCLUDE_JNI_ACCUMULORANGE_H_

#include "AccumuloKey.h"
#include "data/constructs/Range.h"
#include "JavaException.h"
namespace cclient {
namespace jni {

class AccumuloRange {
  jobject range;
  std::shared_ptr<cclient::data::Range> rangeRef = nullptr;
  JNIEnv *env;
  mutable jclass rangeClass;

 public:
  AccumuloRange(JNIEnv *env, jobject range)
      :
      env(env),
      range(range) {
    rangeClass = env->GetObjectClass(range);
    this->range = range;
  }

  bool init(JNIEnv *env) {
    /**
     * load compoonent parts
     */

    auto keyClass = env->FindClass("org/apache/accumulo/core/data/Key");

    auto getStartKeyMethod = env->GetMethodID(rangeClass, "getStartKey", "()Lorg/apache/accumulo/core/data/Key;");
    auto getEndKeyMethod = env->GetMethodID(rangeClass, "getEndKey", "()Lorg/apache/accumulo/core/data/Key;");

    auto isStartKeyInclusiveMethod = env->GetMethodID(rangeClass, "isStartKeyInclusive", "()Z");
    auto isStopKeyInclusiveMethod = env->GetMethodID(rangeClass, "isEndKeyInclusive", "()Z");

    bool isStartKeyInclusive = env->CallBooleanMethod(range, isStartKeyInclusiveMethod) == JNI_TRUE;
    if (env->ExceptionOccurred())  // check if an exception occurred
    {
      env->ExceptionDescribe();  // print the stack trace
      return false;
    }
    bool isStopKeyInclusive = env->CallBooleanMethod(range, isStopKeyInclusiveMethod) == JNI_TRUE;
    if (env->ExceptionOccurred())  // check if an exception occurred
    {
      env->ExceptionDescribe();  // print the stack trace
      return false;
    }

    auto startkey = env->CallObjectMethod(range, getStartKeyMethod);
    if (env->ExceptionOccurred())  // check if an exception occurred
    {
      env->ExceptionDescribe();  // print the stack trace
      return false;
    }
    auto stopkey = env->CallObjectMethod(range, getEndKeyMethod);
    if (env->ExceptionOccurred())  // check if an exception occurred
    {
      env->ExceptionDescribe();  // print the stack trace
      return false;
    }

    std::shared_ptr<cclient::data::Key> keyA, keyB;

    keyA = keyB = nullptr;
    if (startkey != nullptr) {
      AccumuloKey start(env, startkey);

      if (!start.init(env))
        return false;
      keyA = start.toKey();
    }

    if (stopkey != nullptr) {
      AccumuloKey stop(env, stopkey);

      if (!stop.init(env))
        return false;
      keyB = stop.toKey();
    }

    rangeRef = std::make_shared<cclient::data::Range>(keyA, isStartKeyInclusive, keyB, isStopKeyInclusive, false);
    return true;
  }

  AccumuloRange(const std::shared_ptr<cclient::data::Range> &range)
      :
      env(nullptr),
      range(nullptr) {
    rangeRef = range;

  }

  jobject getAccumuloRange(JNIEnv *env) const {
    if (env->ExceptionOccurred())  // check if an exception occurred
    {
      env->ExceptionDescribe();  // print the stack trace
      return nullptr;
    }
    rangeClass = env->FindClass("org/apache/accumulo/core/data/Range");

    if (rangeRef->getInfiniteStartKey())  // assume both inifinite
    {

      auto constructor = env->GetMethodID(rangeClass, "<init>", "()V");
      return env->NewObject(rangeClass, constructor);
    } else {
      auto constructor = env->GetMethodID(rangeClass, "<init>", "(Lorg/apache/accumulo/core/data/Key;ZLorg/apache/accumulo/core/data/Key;Z)V");

      AccumuloKey start(rangeRef->getStartKey());

      AccumuloKey stop(rangeRef->getStopKey());

      jboolean startInclusive = rangeRef->getStartKeyInclusive() ? JNI_TRUE : JNI_FALSE;
      jboolean stopInclusive = rangeRef->getStopKeyInclusive() ? JNI_TRUE : JNI_FALSE;

      if (env->ExceptionOccurred())  // check if an exception occurred
      {
        env->ExceptionDescribe();  // print the stack trace
        return nullptr;
      }
      auto stk = start.getAccumuloKey(env);

      if (stk == nullptr) {
        cclient::jni::JavaException("Coould not create key");
      }
      if (env->ExceptionOccurred())  // check if an exception occurred
      {
        env->ExceptionDescribe();  // print the stack trace
        return nullptr;
      }
      auto stok = stop.getAccumuloKey(env);
      if (stok == nullptr) {
        cclient::jni::JavaException("Coould not create key");
      }
      if (env->ExceptionOccurred())  // check if an exception occurred
      {
        env->ExceptionDescribe();  // print the stack trace
        return nullptr;
      }
      return env->NewObject(rangeClass, constructor, stk, startInclusive, stok, stopInclusive);
    }

  }

  std::shared_ptr<cclient::data::Range> getRange() const {
    return rangeRef;
  }

};

} /** jni **/
} /** cclient **/

#endif /* INCLUDE_JNI_ACCUMULORANGE_H_ */
