CPPFLAGS=-g -std=c++11
LDFLAGS=-g
LDLIBS=
RM=rm -f
PWD=`pwd`

SRCS=../Key.cpp ../rkey.cpp ../column.cpp ../security/AuthInfo.cpp ../security/Authorizations.cpp ConstructHarness.cpp
OBJS=$(subst .cpp,.o,$(SRCS))
DEPENDENCIES=../../streaming/streaming.so 


all: test

clean: 
	$(RM) $(OBJS)


test:	$(OBJS)
	cd ../../streaming ; make clean; make ; cd $(PWD)
	g++ -o construct_test $(OBJS) $(DEPENDENCIES) $(CPPFLAGS)
	$(RM) $(OBJS)
	./construct_test
