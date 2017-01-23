CPPFLAGS=-g --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: tableops

clean: 
	$(RM) $(OBJS)


tableops:	$(OBJS)
	g++ -o tableops.so $(OBJS) $(LDFLAGS) -shared -fPIC
	$(RM) $(OBJS)
