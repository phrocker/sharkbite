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
#ifndef INCLUDE_JNI_ACCUMULOVALUE_H_
#define INCLUDE_JNI_ACCUMULOVALUE_H_

#include <vector>
#include <jni.h>
#include "data/constructs/value.h"
namespace cclient {
namespace jni {

class AccumuloValue {
  jobject value;
  JNIEnv *env;
  mutable jclass valueClass;

  std::vector<uint8_t> valueData;
  jlong ts = 0;
  bool deleted = false;
 public:
  AccumuloValue(JNIEnv *env, jobject value)
      :
      env(env),
      value(value) {
    valueClass = env->GetObjectClass(value);

  }

  bool init(JNIEnv *env) {
    /**
     * load compoonent parts
     */

    auto getValueByteArrayMethod = env->GetMethodID(valueClass, "get", "()[B");

    auto byteArray = (jbyteArray)env->CallObjectMethod(value, getValueByteArrayMethod);
    if (env->ExceptionCheck()) {
      return false;
    }

    auto numBytes = env->GetArrayLength(byteArray);
    uint8_t *data = (uint8_t*) env->GetByteArrayElements(byteArray, nullptr);
    std::copy(data, data + numBytes, std::back_inserter(valueData));
    env->ReleaseByteArrayElements(byteArray, (jbyte*)data, JNI_ABORT);
    env->DeleteLocalRef(byteArray);

    return true;
  }

  AccumuloValue(const std::shared_ptr<cclient::data::Value> &incomingValue)
      :
      env(nullptr),
      value(nullptr) {
    if (incomingValue){
      auto pair = incomingValue->getValue();
      valueData.resize(pair.second);
      memcpy(valueData.data(),pair.first,pair.second);
    }
  }

  std::shared_ptr<cclient::data::Value> toValue() {
    auto cclientValue = std::make_shared<cclient::data::Value>();
    cclientValue->setValue(valueData.data(),valueData.size(),0);
    return cclientValue;
  }

  jobject getAccumuloValue(JNIEnv *env) const {
    valueClass = env->FindClass("org/apache/accumulo/core/data/Value");

    auto len = valueData.size();
    auto javaByteArray = env->NewByteArray( len);
    env->SetByteArrayRegion( javaByteArray, 0, len, (const jbyte*) valueData.data() );
    jobject jvalue = nullptr;

    auto constructor = env->GetMethodID(valueClass, "<init>", "([BZ)V");
    jvalue = env->NewObject(valueClass, constructor, javaByteArray,false);

    return jvalue;

  }


};

} /** jni **/
} /** cclient **/

#endif /* INCLUDE_JNI_ACCUMULOVALUE_H_ */
