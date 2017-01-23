CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: security

clean: 
	$(RM) $(OBJS)


security:	$(OBJS)
	g++ -o security.so $(OBJS) $(LDFLAGS) -shared
	$(RM) $(OBJS)
