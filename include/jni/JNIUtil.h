/**
 *
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

#ifndef _JNIUTIL_H
#define _JNIUTIL_H

#include <string>
#include <jni.h>

static inline std::string JniStringToUTF(JNIEnv *env, jstring jStr) {
  if (!jStr)
    return "";

  const jclass stringClass = env->GetObjectClass(jStr);
  const jmethodID getBytes = env->GetMethodID(stringClass, "getBytes", "(Ljava/lang/String;)[B");
  const jbyteArray stringJbytes = (jbyteArray) env->CallObjectMethod(jStr, getBytes, env->NewStringUTF("UTF-8"));
  if (env->ExceptionCheck()) {
    return "";
  }
  size_t length = (size_t) env->GetArrayLength(stringJbytes);
  jbyte *pBytes = env->GetByteArrayElements(stringJbytes, NULL);

  std::string ret = std::string((char*) pBytes, length);
  env->ReleaseByteArrayElements(stringJbytes, pBytes, JNI_ABORT);

  env->DeleteLocalRef(stringJbytes);
  env->DeleteLocalRef(stringClass);
  return ret;
}


static inline jbyteArray toByteArray(JNIEnv *env, const std::string &str) {
  jbyteArray array = env->NewByteArray(str.length());
  if (env->ExceptionOccurred())  // check if an exception occurred
  {
    throw std::runtime_error("Error while creating byte array");
  }
  env->SetByteArrayRegion(array, 0, str.length(), reinterpret_cast<jbyte*>(const_cast<char*>(str.data())));
  if (env->ExceptionOccurred())  // check if an exception occurred
  {
    throw std::runtime_error("Error while creating byte array");
  }
  return array;
}

// various likely/unlikely pragmas I've carried over the years.
// you'll see this in many projects
#if defined(__GNUC__) && __GNUC__ >= 4
#define LIKELY(x)   (__builtin_expect((x), 1))
#define UNLIKELY(x) (__builtin_expect((x), 0))
#else
#define LIKELY(x)   (x)
#define UNLIKELY(x) (x)
#endif

#endif //_JNIUTIL_H
