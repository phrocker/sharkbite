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

static inline std::string JniStringToUTF(JNIEnv *env, const jstring &jstr) {
  if (!jstr && !env) {
    return "";
  }
  const char *c_str = env->GetStringUTFChars(jstr, NULL);
  if (c_str == NULL) {
    return "";
  }
  std::string str = c_str;
  env->ReleaseStringUTFChars(jstr, c_str);
  return str;
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
